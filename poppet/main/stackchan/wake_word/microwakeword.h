#ifndef MICROWAKEWORD_H
#define MICROWAKEWORD_H

// microWakeWord (TFLite-Micro streaming) wake-word detector.
//
// This is an original, MIT-licensed implementation for the familiar/poppet
// project. It is structurally forked from the vendored AfeWakeWord
// (vendor/stackchan-esp32/main/audio/wake_words/afe_wake_word.cc): the ESP-SR
// AFE front-end setup and the entire pre-roll capture / opus-encode path are
// mirrored from it, but WakeNet detection is replaced by an on-device
// microWakeWord pipeline (ESP-SR AFE for AEC/NS/VAD -> Apache-2.0 mel-feature
// frontend -> Apache-2.0 esp-tflite-micro streaming inference).
//
// The detector algorithm mirrors the Apache-2.0 reference
// OHF-Voice/pymicro-wakeword (process_streaming); no GPLv3 ESPHome code is
// used. See main/stackchan/wake_word/microwakeword_setup.md.

#include <freertos/FreeRTOS.h>
#include <freertos/task.h>
#include <freertos/event_groups.h>

#include <esp_afe_sr_models.h>
#include <model_path.h>

#include <deque>
#include <string>
#include <vector>
#include <functional>
#include <memory>
#include <mutex>
#include <condition_variable>

#include "audio_codec.h"
#include "wake_word.h"

// All TFLite-Micro and mel-frontend state lives in this pimpl, defined in the
// .cc, so their heavy headers never leak into audio_service.cc (which includes
// this header only to construct the object).
struct MwwDetector;

class MicroWakeWord : public WakeWord {
public:
    MicroWakeWord();
    ~MicroWakeWord();

    bool Initialize(AudioCodec* codec, srmodel_list_t* models_list) override;
    void Feed(const std::vector<int16_t>& data) override;
    void OnWakeWordDetected(std::function<void(const std::string& wake_word)> callback) override;
    void Start() override;
    void Stop() override;
    size_t GetFeedSize() override;
    void EncodeWakeWordData() override;
    bool GetWakeWordOpus(std::vector<uint8_t>& opus) override;
    const std::string& GetLastDetectedWakeWord() const override { return last_detected_wake_word_; }

private:
    // --- ESP-SR AFE front-end (AEC/NS/VAD only; wakenet_init = false) ---
    srmodel_list_t* models_ = nullptr;
    bool owns_models_ = false;
    const esp_afe_sr_iface_t* afe_iface_ = nullptr;
    esp_afe_sr_data_t* afe_data_ = nullptr;
    EventGroupHandle_t event_group_ = nullptr;
    AudioCodec* codec_ = nullptr;
    std::function<void(const std::string& wake_word)> wake_word_detected_callback_;
    std::string last_detected_wake_word_;
    std::vector<int16_t> input_buffer_;
    std::mutex input_buffer_mutex_;

    // --- microWakeWord detector (TFLite + mel frontend), pimpl ---
    std::unique_ptr<MwwDetector> detector_;

    // --- pre-roll capture + opus encode (mirrors AfeWakeWord) ---
    TaskHandle_t wake_word_encode_task_ = nullptr;
    StaticTask_t* wake_word_encode_task_buffer_ = nullptr;
    StackType_t* wake_word_encode_task_stack_ = nullptr;
    std::deque<std::vector<int16_t>> wake_word_pcm_;
    std::deque<std::vector<uint8_t>> wake_word_opus_;
    std::mutex wake_word_mutex_;
    std::condition_variable wake_word_cv_;

    void StoreWakeWordData(const int16_t* data, size_t samples);
    void AudioDetectionTask();
};

#endif  // MICROWAKEWORD_H
