#ifndef PROTOCOL_H
#define PROTOCOL_H

#include <cJSON.h>
#include <string>
#include <functional>
#include <chrono>
#include <vector>

struct AudioStreamPacket {
    int sample_rate = 0;
    int frame_duration = 0;
    uint32_t timestamp = 0;
    // True for server→device TTS frames, false for locally generated audio
    // (PlaySound OGG). Only server frames participate in v2 credit flow
    // control (§5): they are credited back when consumed OR dropped, and
    // local frames must never mint credit (that would inflate the server's
    // budget past the device's real buffer).
    bool from_server = false;
    std::vector<uint8_t> payload;
};

struct BinaryProtocol2 {
    uint16_t version;
    uint16_t type;          // Message type (0: OPUS, 1: JSON)
    uint32_t reserved;      // Reserved for future use
    uint32_t timestamp;     // Timestamp in milliseconds (used for server-side AEC)
    uint32_t payload_size;  // Payload size in bytes
    uint8_t payload[];      // Payload data
} __attribute__((packed));

struct BinaryProtocol3 {
    uint8_t type;
    uint8_t reserved;
    uint16_t payload_size;
    uint8_t payload[];
} __attribute__((packed));

enum AbortReason {
    kAbortReasonNone,
    kAbortReasonWakeWordDetected
};

enum ListeningMode {
    kListeningModeAutoStop,
    kListeningModeManualStop,
    kListeningModeRealtime // 需要 AEC 支持
};

class Protocol {
public:
    virtual ~Protocol() = default;

    inline int server_sample_rate() const {
        return server_sample_rate_;
    }
    inline int server_frame_duration() const {
        return server_frame_duration_;
    }
    inline const std::string& session_id() const {
        return session_id_;
    }

    void OnIncomingAudio(std::function<void(std::unique_ptr<AudioStreamPacket> packet)> callback);
    void OnIncomingJson(std::function<void(const cJSON* root)> callback);
    void OnAudioChannelOpened(std::function<void()> callback);
    void OnAudioChannelClosed(std::function<void()> callback);
    void OnNetworkError(std::function<void(const std::string& message)> callback);
    void OnConnected(std::function<void()> callback);
    void OnDisconnected(std::function<void()> callback);

    virtual bool Start() = 0;
    virtual bool OpenAudioChannel() = 0;
    virtual void CloseAudioChannel(bool send_goodbye = true) = 0;
    virtual bool IsAudioChannelOpened() const = 0;
    virtual bool SendAudio(std::unique_ptr<AudioStreamPacket> packet) = 0;
    virtual void SendWakeWordDetected(const std::string& wake_word);
    virtual void SendStartListening(ListeningMode mode);
    virtual void SendStopListening();
    virtual void SendAbortSpeaking(AbortReason reason);
    // Protocol v2: a complete first-class tool frame (tool_list / tool_call
    // response) built by McpServer, sent verbatim. Replaces v1's SendMcpMessage
    // (there is no {type:mcp,...} envelope in v2 — PROTOCOL_V2 §6).
    virtual void SendToolMessage(const std::string& frame);
    // Dotty: ambient perception event frame.
    // Protocol v2: emits {"type":"telemetry","event":<name>,"data":<data_json>}
    // server-bound (§4.8). No session_id — the connection is the session.
    virtual void SendEvent(const std::string& name, const std::string& data_json);

    // Protocol v2 audio flow control (§5): grant the server `frames` more
    // server→device audio frames of send budget.
    virtual void SendAudioCredit(int frames);
    // Called by the audio service when server TTS frames are consumed off the
    // decode queue — or flushed/dropped without playing (barge-in ResetDecoder,
    // state-gate drops), which must also credit back or the server's window
    // shrinks permanently with each interruption. Batches grants and emits
    // audio_credit once a batch accumulates, so the server's send budget never
    // starves.
    void NotifyAudioFrameConsumed(int frames = 1);
    // Reset the unsent-credit accumulator (on channel open/close).
    void ResetAudioCredit();

    // Dotty: exposed for the proactive idle-channel reconnect in the
    // main clock-tick handler. True if no incoming WS frame has arrived
    // in kTimeoutSeconds (120 s).
    virtual bool IsTimeout() const;

protected:
    std::function<void(const cJSON* root)> on_incoming_json_;
    std::function<void(std::unique_ptr<AudioStreamPacket> packet)> on_incoming_audio_;
    std::function<void()> on_audio_channel_opened_;
    std::function<void()> on_audio_channel_closed_;
    std::function<void(const std::string& message)> on_network_error_;
    std::function<void()> on_connected_;
    std::function<void()> on_disconnected_;

    // Fallback if the server hello omits audio.out (it shouldn't — §3.2 dictates
    // it). 16000 matches the device's fixed playback rate; a 24k default here
    // would make a hello-omission silently configure the decoder off-rate.
    int server_sample_rate_ = 16000;
    int server_frame_duration_ = 60;
    bool error_occurred_ = false;
    std::string session_id_;  // v1/MQTT only; unused on the v2 websocket wire
    std::chrono::time_point<std::chrono::steady_clock> last_incoming_time_;

    // Protocol v2 flow control (§5): decoded TTS frames consumed but not yet
    // credited back to the server. Grant once it reaches kAudioCreditBatch.
    static constexpr int kAudioCreditBatch = 8;
    int frames_consumed_since_grant_ = 0;

    virtual bool SendText(const std::string& text) = 0;
    virtual void SetError(const std::string& message);
};

#endif // PROTOCOL_H

