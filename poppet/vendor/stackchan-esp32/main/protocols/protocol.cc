#include "protocol.h"

#include <esp_log.h>

#define TAG "Protocol"

void Protocol::OnIncomingJson(std::function<void(const cJSON* root)> callback) {
    on_incoming_json_ = callback;
}

void Protocol::OnIncomingAudio(std::function<void(std::unique_ptr<AudioStreamPacket> packet)> callback) {
    on_incoming_audio_ = callback;
}

void Protocol::OnAudioChannelOpened(std::function<void()> callback) {
    on_audio_channel_opened_ = callback;
}

void Protocol::OnAudioChannelClosed(std::function<void()> callback) {
    on_audio_channel_closed_ = callback;
}

void Protocol::OnNetworkError(std::function<void(const std::string& message)> callback) {
    on_network_error_ = callback;
}

void Protocol::OnConnected(std::function<void()> callback) {
    on_connected_ = callback;
}

void Protocol::OnDisconnected(std::function<void()> callback) {
    on_disconnected_ = callback;
}

void Protocol::SetError(const std::string& message) {
    error_occurred_ = true;
    if (on_network_error_ != nullptr) {
        on_network_error_(message);
    }
}

// Protocol v2 (§4.2): barge-in / interrupt. No session_id — the connection is
// the session. The wake-word reason is "wake" in v2 (v1 used
// "wake_word_detected").
void Protocol::SendAbortSpeaking(AbortReason reason) {
    std::string message = "{\"type\":\"abort\"";
    if (reason == kAbortReasonWakeWordDetected) {
        message += ",\"reason\":\"wake\"";
    }
    message += "}";
    SendText(message);
}

// Protocol v2 (§4.2): wake-word fired. Split out of v1's overloaded `listen`
// type. `phrase` is the wake-word identifier; `score` is omitted (the AFE path
// does not surface a confidence value here).
void Protocol::SendWakeWordDetected(const std::string& wake_word) {
    std::string json = "{\"type\":\"wake\",\"phrase\":\"" + wake_word + "\"}";
    SendText(json);
}

// Protocol v2 (§4.2): begin a capture window.
void Protocol::SendStartListening(ListeningMode mode) {
    std::string message = "{\"type\":\"listen_start\"";
    if (mode == kListeningModeRealtime) {
        message += ",\"mode\":\"realtime\"";
    } else if (mode == kListeningModeAutoStop) {
        message += ",\"mode\":\"auto\"";
    } else {
        message += ",\"mode\":\"manual\"";
    }
    message += "}";
    SendText(message);
}

// Protocol v2 (§4.2): end a capture window.
void Protocol::SendStopListening() {
    SendText("{\"type\":\"listen_stop\"}");
}

// Protocol v2 (§6): send a complete first-class tool frame verbatim (built by
// McpServer). Replaces v1's {type:mcp,payload:{jsonrpc...}} envelope.
void Protocol::SendToolMessage(const std::string& frame) {
    SendText(frame);
}

// Dotty: ambient perception event frame.
// Protocol v2 (§4.8): a typed telemetry notification. No session_id; the event
// name moves to the `event` field. data_json is the raw event payload object.
void Protocol::SendEvent(const std::string& name, const std::string& data_json) {
    std::string message = "{\"type\":\"telemetry\",\"event\":\"" + name +
                          "\",\"data\":" + data_json + "}";
    SendText(message);
}

// Protocol v2 (§5.2): grant the server `frames` more audio frames of budget.
void Protocol::SendAudioCredit(int frames) {
    if (frames <= 0) {
        return;
    }
    SendText("{\"type\":\"audio_credit\",\"frames\":" + std::to_string(frames) + "}");
}

void Protocol::NotifyAudioFrameConsumed(int frames) {
    if (frames <= 0) {
        return;
    }
    frames_consumed_since_grant_ += frames;
    if (frames_consumed_since_grant_ >= kAudioCreditBatch) {
        SendAudioCredit(frames_consumed_since_grant_);
        frames_consumed_since_grant_ = 0;
    }
}

void Protocol::ResetAudioCredit() {
    frames_consumed_since_grant_ = 0;
}

bool Protocol::IsTimeout() const {
    const int kTimeoutSeconds = 120;
    auto now = std::chrono::steady_clock::now();
    auto duration = std::chrono::duration_cast<std::chrono::seconds>(now - last_incoming_time_);
    bool timeout = duration.count() > kTimeoutSeconds;
    if (timeout) {
        ESP_LOGE(TAG, "Channel timeout %ld seconds", (long)duration.count());
    }
    return timeout;
}
