#include "microwakeword.h"
#if defined(CONFIG_USE_MICROWAKEWORD)

#include "audio_service.h"  // AS_OPUS_ENC_CONFIG, esp_opus_enc_* (shared with AfeWakeWord)

#include <esp_log.h>
#include <esp_timer.h>
#include <esp_partition.h>
#include <esp_heap_caps.h>

#include <cmath>
#include <cstring>
#include <algorithm>

// Apache-2.0 mel-feature frontend (kahrendt/ESPMicroSpeechFeatures).
extern "C" {
#include "frontend.h"
#include "frontend_util.h"
}

// Apache-2.0 esp-tflite-micro.
#include "tensorflow/lite/micro/micro_allocator.h"
#include "tensorflow/lite/micro/micro_interpreter.h"
#include "tensorflow/lite/micro/micro_mutable_op_resolver.h"
#include "tensorflow/lite/micro/micro_resource_variable.h"
#include "tensorflow/lite/schema/schema_generated.h"

#define TAG "MicroWakeWord"
#define DETECTION_RUNNING_EVENT 1

// Feature-frontend constants, from the Apache-2.0 reference
// rhasspy/pymicro-features (micro_features.cpp) — these MUST match training.
static constexpr int kSampleRate = 16000;
static constexpr int kNumChannels = 40;             // mel filterbank channels
static constexpr int kWindowSizeMs = 30;
static constexpr int kStepSizeMs = 10;
static constexpr float kFeatureScale = 0.0390625f;  // = 1/25.6, fixed pre-quant scale

// Number of builtin ops registered below (keep in sync with RegisterOps()).
static constexpr int kNumOps = 13;

// Resource-variable bookkeeping arena (VAR_HANDLE/READ/ASSIGN metadata; the
// variable buffers themselves are counted against this allocator too).
static constexpr size_t kVarArenaSize = 4 * 1024;
static constexpr int kMaxResourceVariables = 8;

// Tensor arena. The manifest's tensor_arena_size (30000 for hey_artemis) counts
// only persistent buffers — AllocateTensors also needs scratch for the
// CALL_ONCE init subgraph (the model failed at 40 KB asking for ~64 KB).
// Allocate generously; actual usage is logged after alloc.
static constexpr size_t kTensorArenaSize = 80 * 1024;

// ---------------------------------------------------------------------------
// Detector pimpl: all TFLite + frontend state, kept out of the header.
// ---------------------------------------------------------------------------
struct MwwDetector {
    // mel frontend
    FrontendState frontend_state{};
    bool frontend_ready = false;

    // model, mmap'd from the mww_model partition (not copied into RAM)
    esp_partition_mmap_handle_t mmap_handle = 0;
    const void* model_data = nullptr;

    // interpreter
    uint8_t* tensor_arena = nullptr;
    uint8_t* var_arena = nullptr;   // holds the MicroAllocator + MicroResourceVariables
    tflite::MicroResourceVariables* resource_variables = nullptr;
    tflite::MicroMutableOpResolver<kNumOps>* resolver = nullptr;
    tflite::MicroInterpreter* interpreter = nullptr;
    TfLiteTensor* input = nullptr;
    TfLiteTensor* output = nullptr;

    // input quantization + shape
    float input_scale = 1.0f;
    int input_zero_point = 0;
    int stride = 1;             // # of 10ms feature frames fed per invoke
    int input_elems = kNumChannels;

    // output quantization
    float output_scale = 1.0f;
    int output_zero_point = 0;

    // streaming detection state
    std::vector<float> feature_accum;   // input_elems floats, already /25.6-scaled
    int frames_in_accum = 0;
    std::deque<float> prob_window;
    float probability_cutoff = 0.95f;
    int sliding_window_size = 5;

    void ResetStreaming() {
        frames_in_accum = 0;
        prob_window.clear();
        if (frontend_ready) {
            FrontendReset(&frontend_state);
        }
        if (resource_variables != nullptr) {
            resource_variables->ResetAll();  // zero the model's temporal state
        }
    }
};

static bool RegisterOps(tflite::MicroMutableOpResolver<kNumOps>* r) {
    // Exact op set of the trained hey_artemis streaming graph
    // (tflite_stream_state_internal_quant; enumerated from the flatbuffer).
    // If AllocateTensors logs "Didn't find op for builtin opcode", add the
    // missing op here and bump kNumOps.
    if (r->AddAssignVariable() != kTfLiteOk) return false;
    if (r->AddCallOnce() != kTfLiteOk) return false;
    if (r->AddConcatenation() != kTfLiteOk) return false;
    if (r->AddConv2D() != kTfLiteOk) return false;
    if (r->AddDepthwiseConv2D() != kTfLiteOk) return false;
    if (r->AddFullyConnected() != kTfLiteOk) return false;
    if (r->AddLogistic() != kTfLiteOk) return false;
    if (r->AddQuantize() != kTfLiteOk) return false;
    if (r->AddReadVariable() != kTfLiteOk) return false;
    if (r->AddReshape() != kTfLiteOk) return false;
    if (r->AddSplitV() != kTfLiteOk) return false;
    if (r->AddStridedSlice() != kTfLiteOk) return false;
    if (r->AddVarHandle() != kTfLiteOk) return false;
    return true;
}

// ---------------------------------------------------------------------------
// MicroWakeWord
// ---------------------------------------------------------------------------
MicroWakeWord::MicroWakeWord()
    : last_detected_wake_word_("Hey Artemis"),
      wake_word_pcm_(),
      wake_word_opus_() {
    event_group_ = xEventGroupCreate();
    detector_ = std::make_unique<MwwDetector>();
}

MicroWakeWord::~MicroWakeWord() {
    if (afe_data_ != nullptr) {
        afe_iface_->destroy(afe_data_);
    }
    if (wake_word_encode_task_stack_ != nullptr) {
        heap_caps_free(wake_word_encode_task_stack_);
    }
    if (wake_word_encode_task_buffer_ != nullptr) {
        heap_caps_free(wake_word_encode_task_buffer_);
    }
    if (detector_) {
        delete detector_->interpreter;
        delete detector_->resolver;
        if (detector_->tensor_arena) heap_caps_free(detector_->tensor_arena);
        if (detector_->var_arena) heap_caps_free(detector_->var_arena);  // frees allocator + mrv
        if (detector_->frontend_ready) FrontendFreeStateContents(&detector_->frontend_state);
        if (detector_->model_data) esp_partition_munmap(detector_->mmap_handle);
    }
    if (owns_models_ && models_ != nullptr) {
        esp_srmodel_deinit(models_);
    }
    if (event_group_ != nullptr) {
        vEventGroupDelete(event_group_);
    }
}

bool MicroWakeWord::Initialize(AudioCodec* codec, srmodel_list_t* models_list) {
    codec_ = codec;
    int ref_num = codec_->input_reference() ? 1 : 0;

    if (models_list == nullptr) {
        models_ = esp_srmodel_init("model");
        owns_models_ = true;
    } else {
        models_ = models_list;
        owns_models_ = false;
    }

    // ---- ESP-SR AFE front-end: AEC/NS/VAD only, WakeNet disabled ----
    std::string input_format;
    for (int i = 0; i < codec_->input_channels() - ref_num; i++) {
        input_format.push_back('M');
    }
    for (int i = 0; i < ref_num; i++) {
        input_format.push_back('R');
    }
    afe_config_t* afe_config = afe_config_init(input_format.c_str(), models_, AFE_TYPE_SR, AFE_MODE_HIGH_PERF);
    // No AEC: wake detection only runs while idle (half-duplex v1 — nothing is
    // playing), and AEC SR_HIGH_PERF preempts the inference task on core 1,
    // pushing invoke past its real-time budget (measured 30.7ms vs 20ms).
    afe_config->aec_init = false;
    afe_config->wakenet_init = false;   // <-- key difference from AfeWakeWord
    afe_config->afe_perferred_core = 1;
    afe_config->afe_perferred_priority = 1;
    afe_config->memory_alloc_mode = AFE_MEMORY_ALLOC_MORE_PSRAM;

    afe_iface_ = esp_afe_handle_from_config(afe_config);
    afe_data_ = afe_iface_->create_from_config(afe_config);
    if (afe_data_ == nullptr) {
        ESP_LOGE(TAG, "Failed to create AFE data");
        return false;
    }

    // ---- mel-feature frontend ----
    MwwDetector* d = detector_.get();
    FrontendConfig fe{};
    FrontendFillConfigWithDefaults(&fe);
    fe.window.size_ms = kWindowSizeMs;
    fe.window.step_size_ms = kStepSizeMs;
    fe.filterbank.num_channels = kNumChannels;
    fe.filterbank.lower_band_limit = 125.0f;
    fe.filterbank.upper_band_limit = 7500.0f;
    fe.noise_reduction.smoothing_bits = 10;
    fe.noise_reduction.even_smoothing = 0.025f;
    fe.noise_reduction.odd_smoothing = 0.06f;
    fe.noise_reduction.min_signal_remaining = 0.05f;
    fe.pcan_gain_control.enable_pcan = 1;
    fe.pcan_gain_control.strength = 0.95f;
    fe.pcan_gain_control.offset = 80.0f;
    fe.pcan_gain_control.gain_bits = 21;
    fe.log_scale.enable_log = 1;
    fe.log_scale.scale_shift = 6;
    if (FrontendPopulateState(&fe, &d->frontend_state, kSampleRate) != 1) {
        ESP_LOGE(TAG, "FrontendPopulateState failed");
        return false;
    }
    d->frontend_ready = true;

    // ---- model: mmap from the mww_model partition ----
    const esp_partition_t* part = esp_partition_find_first(
        ESP_PARTITION_TYPE_DATA, ESP_PARTITION_SUBTYPE_ANY, "mww_model");
    if (part == nullptr) {
        ESP_LOGE(TAG, "mww_model partition not found");
        return false;
    }
    if (esp_partition_mmap(part, 0, part->size, ESP_PARTITION_MMAP_DATA,
                           &d->model_data, &d->mmap_handle) != ESP_OK) {
        ESP_LOGE(TAG, "Failed to mmap mww_model partition");
        return false;
    }
    const tflite::Model* model = tflite::GetModel(d->model_data);
    if (model->version() != TFLITE_SCHEMA_VERSION) {
        ESP_LOGE(TAG, "Model schema %lu != supported %d — is the mww_model partition flashed?",
                 (unsigned long)model->version(), TFLITE_SCHEMA_VERSION);
        return false;
    }

    // ---- interpreter ----
    d->tensor_arena = (uint8_t*)heap_caps_malloc(kTensorArenaSize, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    if (d->tensor_arena == nullptr) {
        d->tensor_arena = (uint8_t*)heap_caps_malloc(kTensorArenaSize, MALLOC_CAP_SPIRAM);
    }
    if (d->tensor_arena == nullptr) {
        ESP_LOGE(TAG, "Failed to allocate %u-byte tensor arena", (unsigned)kTensorArenaSize);
        return false;
    }
    d->resolver = new tflite::MicroMutableOpResolver<kNumOps>();
    if (!RegisterOps(d->resolver)) {
        ESP_LOGE(TAG, "Failed to register TFLite ops");
        return false;
    }
    // The v2 streaming graph keeps its temporal state in resource variables
    // (VAR_HANDLE/READ/ASSIGN, initialized via CALL_ONCE) — the interpreter
    // needs a MicroResourceVariables container. Both the MicroAllocator and
    // the container live inside var_arena, so freeing it releases everything.
    d->var_arena = (uint8_t*)heap_caps_malloc(kVarArenaSize, MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT);
    if (d->var_arena == nullptr) {
        d->var_arena = (uint8_t*)heap_caps_malloc(kVarArenaSize, MALLOC_CAP_SPIRAM);
    }
    if (d->var_arena == nullptr) {
        ESP_LOGE(TAG, "Failed to allocate %u-byte variable arena", (unsigned)kVarArenaSize);
        return false;
    }
    tflite::MicroAllocator* var_allocator =
        tflite::MicroAllocator::Create(d->var_arena, kVarArenaSize);
    d->resource_variables =
        tflite::MicroResourceVariables::Create(var_allocator, kMaxResourceVariables);
    if (d->resource_variables == nullptr) {
        ESP_LOGE(TAG, "Failed to create resource variables");
        return false;
    }
    d->interpreter = new tflite::MicroInterpreter(model, *d->resolver, d->tensor_arena,
                                                  kTensorArenaSize, d->resource_variables);
    if (d->interpreter->AllocateTensors() != kTfLiteOk) {
        ESP_LOGE(TAG, "AllocateTensors failed (arena too small, or a missing op)");
        return false;
    }
    d->input = d->interpreter->input(0);
    d->output = d->interpreter->output(0);
    // mWW v2: int8 features in, probability out as int8 or uint8
    // (hey_artemis emits uint8, scale 1/256 zp 0).
    if (d->input->type != kTfLiteInt8 ||
        (d->output->type != kTfLiteInt8 && d->output->type != kTfLiteUInt8)) {
        ESP_LOGE(TAG, "Expected int8 input / int8|uint8 output tensors (mWW v2)");
        return false;
    }

    // input shape -> stride (# of 10ms frames stacked per invoke)
    int total = 1;
    for (int i = 0; i < d->input->dims->size; i++) total *= d->input->dims->data[i];
    if (total % kNumChannels != 0) {
        ESP_LOGE(TAG, "Input tensor size %d not a multiple of %d channels", total, kNumChannels);
        return false;
    }
    d->input_elems = total;
    d->stride = total / kNumChannels;
    d->feature_accum.assign(total, 0.0f);

    d->input_scale = d->input->params.scale;
    d->input_zero_point = d->input->params.zero_point;
    d->output_scale = d->output->params.scale;
    d->output_zero_point = d->output->params.zero_point;

    d->probability_cutoff = (float)CONFIG_MICROWAKEWORD_THRESHOLD / 100.0f;
    d->sliding_window_size = CONFIG_MICROWAKEWORD_SLIDING_WINDOW;
    d->ResetStreaming();

    ESP_LOGI(TAG,
             "microWakeWord ready: stride=%d input_elems=%d arena_used=%u/%u B "
             "cutoff=%.2f window=%d",
             d->stride, d->input_elems, (unsigned)d->interpreter->arena_used_bytes(),
             (unsigned)kTensorArenaSize, d->probability_cutoff, d->sliding_window_size);

    // ---- detection task ----
    xTaskCreate([](void* arg) {
        auto this_ = (MicroWakeWord*)arg;
        this_->AudioDetectionTask();
        vTaskDelete(NULL);
    }, "mww_detect", 4096, this, 3, nullptr);

    return true;
}

void MicroWakeWord::OnWakeWordDetected(std::function<void(const std::string& wake_word)> callback) {
    wake_word_detected_callback_ = callback;
}

void MicroWakeWord::Start() {
    xEventGroupSetBits(event_group_, DETECTION_RUNNING_EVENT);
}

void MicroWakeWord::Stop() {
    xEventGroupClearBits(event_group_, DETECTION_RUNNING_EVENT);

    std::lock_guard<std::mutex> lock(input_buffer_mutex_);
    if (afe_data_ != nullptr) {
        afe_iface_->reset_buffer(afe_data_);
    }
    input_buffer_.clear();
    if (detector_) {
        detector_->ResetStreaming();
    }
}

void MicroWakeWord::Feed(const std::vector<int16_t>& data) {
    if (afe_data_ == nullptr) {
        return;
    }
    std::lock_guard<std::mutex> lock(input_buffer_mutex_);
    // Check running state inside lock to avoid TOCTOU race with Stop().
    if (!(xEventGroupGetBits(event_group_) & DETECTION_RUNNING_EVENT)) {
        return;
    }
    input_buffer_.insert(input_buffer_.end(), data.begin(), data.end());
    size_t chunk_size = afe_iface_->get_feed_chunksize(afe_data_) * codec_->input_channels();
    while (input_buffer_.size() >= chunk_size) {
        afe_iface_->feed(afe_data_, input_buffer_.data());
        input_buffer_.erase(input_buffer_.begin(), input_buffer_.begin() + chunk_size);
    }
}

size_t MicroWakeWord::GetFeedSize() {
    if (afe_data_ == nullptr) {
        return 0;
    }
    return afe_iface_->get_feed_chunksize(afe_data_);
}

void MicroWakeWord::AudioDetectionTask() {
    auto fetch_size = afe_iface_->get_fetch_chunksize(afe_data_);
    auto feed_size = afe_iface_->get_feed_chunksize(afe_data_);
    ESP_LOGI(TAG, "mWW detection task started, feed size: %d fetch size: %d", feed_size, fetch_size);

    MwwDetector* d = detector_.get();
    while (true) {
        xEventGroupWaitBits(event_group_, DETECTION_RUNNING_EVENT, pdFALSE, pdTRUE, portMAX_DELAY);

        auto res = afe_iface_->fetch_with_delay(afe_data_, portMAX_DELAY);
        if (res == nullptr || res->ret_value == ESP_FAIL) {
            continue;
        }

        // Keep the pre-roll capture identical to AfeWakeWord (AEC/NS-cleaned PCM).
        StoreWakeWordData(res->data, res->data_size / sizeof(int16_t));

        // ---- microWakeWord streaming inference over the cleaned PCM ----
        bool detected = false;
        const int16_t* p = res->data;
        size_t remaining = res->data_size / sizeof(int16_t);
        while (remaining > 0) {
            size_t read = 0;
            FrontendOutput out = FrontendProcessSamples(&d->frontend_state, p, remaining, &read);
            if (read == 0) {
                break;  // frontend needs more samples than we have; wait for next fetch
            }
            p += read;
            remaining -= read;

            if (out.size == 0 || out.values == nullptr) {
                continue;  // window not complete yet this step
            }

            // One new 10ms feature frame: scale uint16 -> float (fixed /25.6).
            int base = d->frames_in_accum * kNumChannels;
            int n = std::min((int)out.size, kNumChannels);
            for (int i = 0; i < n; i++) {
                d->feature_accum[base + i] = (float)out.values[i] * kFeatureScale;
            }
            d->frames_in_accum++;
            if (d->frames_in_accum < d->stride) {
                continue;  // need `stride` frames before an inference
            }

            // Quantize accumulated features into the int8 input tensor
            // (model's own scale/zero-point, read from the .tflite).
            int8_t* in = d->input->data.int8;
            for (int i = 0; i < d->input_elems; i++) {
                int q = (int)lroundf(d->feature_accum[i] / d->input_scale) + d->input_zero_point;
                q = std::max(-128, std::min(127, q));
                in[i] = (int8_t)q;
            }
            d->frames_in_accum = 0;

            int64_t t0 = esp_timer_get_time();
            if (d->interpreter->Invoke() != kTfLiteOk) {
                ESP_LOGW(TAG, "Invoke failed");
                continue;
            }
            // Real-time budget is stride*10ms per invoke; log the average so
            // regressions (e.g. arena falling back to PSRAM) are visible.
            static int64_t s_invoke_us_accum = 0;
            static int s_invoke_count = 0;
            s_invoke_us_accum += esp_timer_get_time() - t0;
            if (++s_invoke_count >= 500) {
                ESP_LOGI(TAG, "invoke avg %d us over %d runs (budget %d ms)",
                         (int)(s_invoke_us_accum / s_invoke_count), s_invoke_count, d->stride * 10);
                s_invoke_us_accum = 0;
                s_invoke_count = 0;
            }
            int raw = (d->output->type == kTfLiteUInt8)
                          ? (int)d->output->data.uint8[0]
                          : (int)d->output->data.int8[0];
            float prob = (raw - d->output_zero_point) * d->output_scale;

            d->prob_window.push_back(prob);
            if ((int)d->prob_window.size() > d->sliding_window_size) {
                d->prob_window.pop_front();
            }
            if ((int)d->prob_window.size() == d->sliding_window_size) {
                float sum = 0.0f;
                for (float x : d->prob_window) sum += x;
                float avg = sum / d->sliding_window_size;
                // Diagnostic: surface near-misses so threshold margins are
                // visible in the log during on-device testing.
                if (avg > 0.4f) {
                    static int64_t s_last_prob_log = 0;
                    int64_t now = esp_timer_get_time();
                    if (now - s_last_prob_log > 300000) {
                        s_last_prob_log = now;
                        ESP_LOGI(TAG, "prob window avg %.3f (cutoff %.2f)", avg, d->probability_cutoff);
                    }
                }
                if (avg > d->probability_cutoff) {
                    ESP_LOGI(TAG, "DETECT: window avg %.3f > cutoff %.2f", avg, d->probability_cutoff);
                    detected = true;
                    break;
                }
            }
        }

        if (detected) {
            Stop();  // also resets streaming state (cooldown until re-armed)
            if (wake_word_detected_callback_) {
                wake_word_detected_callback_(last_detected_wake_word_);
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Pre-roll capture + opus encode — mirrored verbatim from AfeWakeWord.
// ---------------------------------------------------------------------------
void MicroWakeWord::StoreWakeWordData(const int16_t* data, size_t samples) {
    // Mutex-protected: the encoder task also iterates wake_word_pcm_.
    std::lock_guard<std::mutex> lock(wake_word_mutex_);
    wake_word_pcm_.emplace_back(std::vector<int16_t>(data, data + samples));
    // keep ~2 seconds (detect duration 30ms, 16kHz, chunksize 512)
    while (wake_word_pcm_.size() > 2000 / 30) {
        wake_word_pcm_.pop_front();
    }
}

void MicroWakeWord::EncodeWakeWordData() {
    const size_t stack_size = 4096 * 6;
    {
        std::lock_guard<std::mutex> lock(wake_word_mutex_);
        wake_word_opus_.clear();
    }
    if (wake_word_encode_task_stack_ == nullptr) {
        wake_word_encode_task_stack_ = (StackType_t*)heap_caps_malloc(stack_size, MALLOC_CAP_SPIRAM);
        assert(wake_word_encode_task_stack_ != nullptr);
    }
    if (wake_word_encode_task_buffer_ == nullptr) {
        wake_word_encode_task_buffer_ = (StaticTask_t*)heap_caps_malloc(sizeof(StaticTask_t), MALLOC_CAP_INTERNAL);
        assert(wake_word_encode_task_buffer_ != nullptr);
    }

    wake_word_encode_task_ = xTaskCreateStatic([](void* arg) {
        auto this_ = (MicroWakeWord*)arg;
        {
            auto start_time = esp_timer_get_time();
            esp_opus_enc_config_t opus_enc_cfg = AS_OPUS_ENC_CONFIG();
            void* encoder_handle = nullptr;
            auto ret = esp_opus_enc_open(&opus_enc_cfg, sizeof(esp_opus_enc_config_t), &encoder_handle);
            if (encoder_handle == nullptr) {
                ESP_LOGE(TAG, "Failed to create audio encoder, error code: %d", ret);
                std::lock_guard<std::mutex> lock(this_->wake_word_mutex_);
                this_->wake_word_opus_.push_back(std::vector<uint8_t>());
                this_->wake_word_cv_.notify_all();
                return;
            }

            int frame_size = 0;
            int outbuf_size = 0;
            esp_opus_enc_get_frame_size(encoder_handle, &frame_size, &outbuf_size);
            frame_size = frame_size / sizeof(int16_t);

            int packets = 0;
            std::vector<int16_t> in_buffer;
            esp_audio_enc_in_frame_t in = {};
            esp_audio_enc_out_frame_t out = {};

            std::deque<std::vector<int16_t>> local_pcm;
            {
                std::lock_guard<std::mutex> lock(this_->wake_word_mutex_);
                local_pcm.swap(this_->wake_word_pcm_);
            }

            for (auto& pcm : local_pcm) {
                if (in_buffer.empty()) {
                    in_buffer = std::move(pcm);
                } else {
                    in_buffer.reserve(in_buffer.size() + pcm.size());
                    in_buffer.insert(in_buffer.end(), pcm.begin(), pcm.end());
                }

                while (in_buffer.size() >= (size_t)frame_size) {
                    std::vector<uint8_t> opus_buf(outbuf_size);
                    in.buffer = (uint8_t*)(in_buffer.data());
                    in.len = (uint32_t)(frame_size * sizeof(int16_t));
                    out.buffer = opus_buf.data();
                    out.len = outbuf_size;
                    out.encoded_bytes = 0;

                    ret = esp_opus_enc_process(encoder_handle, &in, &out);
                    if (ret == ESP_AUDIO_ERR_OK) {
                        std::lock_guard<std::mutex> lock(this_->wake_word_mutex_);
                        this_->wake_word_opus_.emplace_back(opus_buf.data(), opus_buf.data() + out.encoded_bytes);
                        this_->wake_word_cv_.notify_all();
                        packets++;
                    } else {
                        ESP_LOGE(TAG, "Failed to encode audio, error code: %d", ret);
                    }
                    in_buffer.erase(in_buffer.begin(), in_buffer.begin() + frame_size);
                }
            }
            esp_opus_enc_close(encoder_handle);
            auto end_time = esp_timer_get_time();
            ESP_LOGI(TAG, "Encode wake word opus %d packets in %ld ms", packets, (long)((end_time - start_time) / 1000));

            std::lock_guard<std::mutex> lock(this_->wake_word_mutex_);
            this_->wake_word_opus_.push_back(std::vector<uint8_t>());
            this_->wake_word_cv_.notify_all();
        }
        vTaskDelete(NULL);
    }, "encode_mww", stack_size, this, 2, wake_word_encode_task_stack_, wake_word_encode_task_buffer_);
}

bool MicroWakeWord::GetWakeWordOpus(std::vector<uint8_t>& opus) {
    std::unique_lock<std::mutex> lock(wake_word_mutex_);
    wake_word_cv_.wait(lock, [this]() {
        return !wake_word_opus_.empty();
    });
    opus.swap(wake_word_opus_.front());
    wake_word_opus_.pop_front();
    return !opus.empty();
}

#endif  // CONFIG_USE_MICROWAKEWORD
