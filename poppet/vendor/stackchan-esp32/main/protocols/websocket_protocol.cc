#include "websocket_protocol.h"
#include "board.h"
#include "system_info.h"
#include "application.h"
#include "settings.h"

#include <cstring>
#include <cJSON.h>
#include <esp_log.h>
#include <sdkconfig.h>
#include <esp_app_desc.h>
#include <sys/time.h>
#include "assets/lang_config.h"

#define TAG "WS"

WebsocketProtocol::WebsocketProtocol() {
    event_group_handle_ = xEventGroupCreate();
}

WebsocketProtocol::~WebsocketProtocol() {
    vEventGroupDelete(event_group_handle_);
}

bool WebsocketProtocol::Start() {
    // Only connect to server when audio channel is needed
    return true;
}

bool WebsocketProtocol::SendAudio(std::unique_ptr<AudioStreamPacket> packet) {
    if (websocket_ == nullptr || !websocket_->IsConnected()) {
        return false;
    }

    // Protocol v2 (§7): one binary framing — the raw Opus payload, no header,
    // no length prefix (WS frames are already length-delimited). The v1
    // BinaryProtocol2/3 variants are gone.
    return websocket_->Send(packet->payload.data(), packet->payload.size(), true);
}

bool WebsocketProtocol::SendText(const std::string& text) {
    if (websocket_ == nullptr || !websocket_->IsConnected()) {
        return false;
    }

    if (!websocket_->Send(text)) {
        ESP_LOGE(TAG, "Failed to send text: %s", text.c_str());
        SetError(Lang::Strings::SERVER_ERROR);
        return false;
    }

    return true;
}

bool WebsocketProtocol::IsAudioChannelOpened() const {
    return websocket_ != nullptr && websocket_->IsConnected() && !error_occurred_ && !IsTimeout();
}

void WebsocketProtocol::CloseAudioChannel(bool send_goodbye) {
    (void)send_goodbye;  // Websocket doesn't need to send goodbye message
    ResetAudioCredit();  // v2: drop any unsent flow-control credit (§5)
    websocket_.reset();
    vTaskDelay(pdMS_TO_TICKS(200));
}

bool WebsocketProtocol::OpenAudioChannel() {
    Settings settings("websocket", false);
    std::string url = settings.GetString("url");
    std::string token = settings.GetString("token");

    error_occurred_ = false;
    ResetAudioCredit();  // v2: fresh flow-control budget per channel (§5)

    auto network = Board::GetInstance().GetNetwork();
    websocket_ = network->CreateWebSocket(1);
    if (websocket_ == nullptr) {
        ESP_LOGE(TAG, "Failed to create websocket");
        return false;
    }

    if (!token.empty()) {
        // If token not has a space, add "Bearer " prefix
        if (token.find(" ") == std::string::npos) {
            token = "Bearer " + token;
        }
        websocket_->SetHeader("Authorization", token.c_str());
    }
    websocket_->SetHeader("Protocol-Version", "2");
    websocket_->SetHeader("Device-Id", SystemInfo::GetMacAddress().c_str());
    websocket_->SetHeader("Client-Id", Board::GetInstance().GetUuid().c_str());

    websocket_->OnData([this](const char* data, size_t len, bool binary) {
        if (binary) {
            // Protocol v2 (§7): binary frames are raw Opus, no header.
            if (on_incoming_audio_ != nullptr) {
                on_incoming_audio_(std::make_unique<AudioStreamPacket>(AudioStreamPacket{
                    .sample_rate = server_sample_rate_,
                    .frame_duration = server_frame_duration_,
                    .timestamp = 0,
                    .payload = std::vector<uint8_t>((uint8_t*)data, (uint8_t*)data + len)
                }));
            }
        } else {
            // Parse JSON data. The transport hands us a raw (data, len) view
            // with NO NUL terminator — cJSON_Parse and %s both run strlen past
            // the end otherwise (heap OOB read, sporadic crash). Copy into a
            // terminated string first.
            std::string json(data, len);
            auto root = cJSON_Parse(json.c_str());
            auto type = cJSON_GetObjectItem(root, "type");
            if (cJSON_IsString(type)) {
                if (strcmp(type->valuestring, "hello") == 0) {
                    ParseServerHello(root);
                } else {
                    if (on_incoming_json_ != nullptr) {
                        on_incoming_json_(root);
                    }
                }
            } else {
                // Malformed frame (unparseable JSON or no `type`): report a
                // standalone PROTOCOL_VIOLATION (§9.4) so the server gets
                // visibility, and keep the session alive. Frames with an
                // *unknown* type string are NOT errors — receivers must
                // tolerate those for forward compatibility (§4.1); the
                // dispatcher just logs them.
                ESP_LOGE(TAG, "Missing message type, data: %s", json.c_str());
                SendText(root == nullptr
                             ? R"({"type":"error","code":"PROTOCOL_VIOLATION","message":"unparseable JSON frame"})"
                             : R"({"type":"error","code":"PROTOCOL_VIOLATION","message":"frame missing type field"})");
            }
            cJSON_Delete(root);
        }
        last_incoming_time_ = std::chrono::steady_clock::now();
    });

    websocket_->OnDisconnected([this]() {
        ESP_LOGI(TAG, "Websocket disconnected");
        if (on_audio_channel_closed_ != nullptr) {
            on_audio_channel_closed_();
        }
    });

    ESP_LOGI(TAG, "Connecting to websocket server: %s (Protocol-Version: 2)", url.c_str());
    if (!websocket_->Connect(url.c_str())) {
        ESP_LOGE(TAG, "Failed to connect to websocket server, code=%d", websocket_->GetLastError());
        SetError(Lang::Strings::SERVER_NOT_CONNECTED);
        return false;
    }

    // Send hello message to describe the client
    auto message = GetHelloMessage();
    if (!SendText(message)) {
        return false;
    }

    // Wait for server hello
    EventBits_t bits = xEventGroupWaitBits(event_group_handle_, WEBSOCKET_PROTOCOL_SERVER_HELLO_EVENT, pdTRUE, pdFALSE, pdMS_TO_TICKS(10000));
    if (!(bits & WEBSOCKET_PROTOCOL_SERVER_HELLO_EVENT)) {
        ESP_LOGE(TAG, "Failed to receive server hello");
        SetError(Lang::Strings::SERVER_TIMEOUT);
        return false;
    }

    if (on_audio_channel_opened_ != nullptr) {
        on_audio_channel_opened_();
    }

    return true;
}

std::string WebsocketProtocol::GetHelloMessage() {
    // Protocol v2 client hello (§3.1): client{}, audio{in,out}, features, and
    // the telemetry events this firmware can emit. No session_id, no transport
    // field (the connection IS the session, and the path/header already
    // selected websocket + v2).
    cJSON* root = cJSON_CreateObject();
    cJSON_AddStringToObject(root, "type", "hello");
    cJSON_AddNumberToObject(root, "id", 1);

    cJSON* client = cJSON_CreateObject();
    cJSON_AddStringToObject(client, "name", "stack-chan");
    auto app_desc = esp_app_get_description();
    cJSON_AddStringToObject(client, "version", app_desc != nullptr ? app_desc->version : "0.0.0");
    cJSON_AddStringToObject(client, "device_id", SystemInfo::GetMacAddress().c_str());
    cJSON_AddStringToObject(client, "uuid", Board::GetInstance().GetUuid().c_str());
    cJSON_AddItemToObject(root, "client", client);

    // The server dictates audio params (§3.2); these are the device's fixed
    // hardware: 16k mic in, 16k TTS out (playback is AEC-locked to the mic
    // rate — PROTOCOL_V2 §1), 60ms Opus frames.
    cJSON* audio = cJSON_CreateObject();
    cJSON* in = cJSON_CreateObject();
    cJSON_AddStringToObject(in, "codec", "opus");
    cJSON_AddNumberToObject(in, "rate", 16000);
    cJSON_AddNumberToObject(in, "channels", 1);
    cJSON_AddNumberToObject(in, "frame_ms", OPUS_FRAME_DURATION_MS);
    cJSON_AddItemToObject(audio, "in", in);
    cJSON* out = cJSON_CreateObject();
    cJSON_AddStringToObject(out, "codec", "opus");
    cJSON_AddNumberToObject(out, "rate", 16000);
    cJSON_AddNumberToObject(out, "channels", 1);
    cJSON_AddNumberToObject(out, "frame_ms", OPUS_FRAME_DURATION_MS);
    cJSON_AddItemToObject(audio, "out", out);
    cJSON_AddItemToObject(root, "audio", audio);

    // features: v2 retires v1's "mcp"/"aec" tokens — tools are first-class (§6).
    cJSON* features = cJSON_CreateArray();
    cJSON_AddItemToArray(features, cJSON_CreateString("tools"));
#if CONFIG_SEND_WAKE_WORD_DATA
    // wake_word_audio (§4.2): we stream the buffered pre-roll after wake, so
    // the server must open the turn's audio window at wake rather than at
    // listen_start. Advertised only when the pre-roll path is compiled in —
    // without the advert the server discards pre-wake frames as orphans.
    cJSON_AddItemToArray(features, cJSON_CreateString("wake_word_audio"));
#endif
    cJSON_AddItemToObject(root, "features", features);

    // telemetry_events: advisory list of perception events this firmware emits
    // (§4.8). The server logs unknown events; it acts on a recognized subset.
    static const char* kTelemetryEvents[] = {
        "face_detected", "face_lost", "face_identified_applied",
        "face_identified_rejected", "sound_event", "head_pet_started",
        "head_pet_ended", "state_changed", "chat_status", "idle_motion",
        "dance_started", "dance_ended", "sleep_pose", "security_pose",
        "battery_low",  // §4.8: the one event the server acts on (→ alert)
    };
    cJSON* telemetry_events = cJSON_CreateArray();
    for (auto* ev : kTelemetryEvents) {
        cJSON_AddItemToArray(telemetry_events, cJSON_CreateString(ev));
    }
    cJSON_AddItemToObject(root, "telemetry_events", telemetry_events);

    auto json_str = cJSON_PrintUnformatted(root);
    std::string message(json_str);
    cJSON_free(json_str);
    cJSON_Delete(root);
    return message;
}

void WebsocketProtocol::ParseServerHello(const cJSON* root) {
    // Protocol v2 server hello (§3.2). result must be "ok"; an error means the
    // server rejected the handshake (e.g. UNSUPPORTED_AUDIO).
    auto result = cJSON_GetObjectItem(root, "result");
    if (cJSON_IsString(result) && strcmp(result->valuestring, "ok") != 0) {
        ESP_LOGE(TAG, "Server hello result: %s", result->valuestring);
        SetError(Lang::Strings::SERVER_ERROR);
        return;
    }
    auto error = cJSON_GetObjectItem(root, "error");
    if (cJSON_IsObject(error)) {
        auto code = cJSON_GetObjectItem(error, "code");
        auto message = cJSON_GetObjectItem(error, "message");
        ESP_LOGE(TAG, "Server rejected hello: %s: %s",
                 cJSON_IsString(code) ? code->valuestring : "?",
                 cJSON_IsString(message) ? message->valuestring : "?");
        SetError(Lang::Strings::SERVER_ERROR);
        return;
    }

    // audio.out is what the server will send (TTS); bind our decoder to it.
    auto audio = cJSON_GetObjectItem(root, "audio");
    if (cJSON_IsObject(audio)) {
        auto out = cJSON_GetObjectItem(audio, "out");
        if (cJSON_IsObject(out)) {
            auto rate = cJSON_GetObjectItem(out, "rate");
            if (cJSON_IsNumber(rate)) {
                server_sample_rate_ = rate->valueint;
            }
            auto frame_ms = cJSON_GetObjectItem(out, "frame_ms");
            if (cJSON_IsNumber(frame_ms)) {
                server_frame_duration_ = frame_ms->valueint;
            }
        }
    }

    // time moves into the hello in v2 (one fewer round-trip than v1's OTA
    // server_time). Set the system clock, mirroring Ota::ParseServerTime:
    // fold the tz offset into the epoch so localtime reads correctly without a
    // TZ env var, matching how the rest of the firmware treats time.
    auto time_obj = cJSON_GetObjectItem(root, "time");
    if (cJSON_IsObject(time_obj)) {
        auto unix_ms = cJSON_GetObjectItem(time_obj, "unix_ms");
        if (cJSON_IsNumber(unix_ms)) {
            double ts = unix_ms->valuedouble;
            auto tz_offset_min = cJSON_GetObjectItem(time_obj, "tz_offset_min");
            if (cJSON_IsNumber(tz_offset_min)) {
                ts += (tz_offset_min->valueint * 60.0 * 1000.0);
            }
            struct timeval tv;
            tv.tv_sec = (time_t)(ts / 1000);
            tv.tv_usec = (suseconds_t)((long long)ts % 1000) * 1000;
            settimeofday(&tv, NULL);
            ESP_LOGI(TAG, "Clock set from server hello");
        }
    }

    // flow_control.audio_credit_initial is the server's starting send budget
    // (§5.1); the device replenishes it as it plays frames (NotifyAudioFrame
    // Consumed). Logged for visibility; no device-side counter is needed.
    auto flow_control = cJSON_GetObjectItem(root, "flow_control");
    if (cJSON_IsObject(flow_control)) {
        auto initial = cJSON_GetObjectItem(flow_control, "audio_credit_initial");
        if (cJSON_IsNumber(initial)) {
            ESP_LOGI(TAG, "Server audio_credit_initial: %d", initial->valueint);
        }
    }

    xEventGroupSetBits(event_group_handle_, WEBSOCKET_PROTOCOL_SERVER_HELLO_EVENT);
}
