// SPDX-License-Identifier: AGPL-3.0-or-later

package protov2

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateExamples regenerates the testdata/examples/*.json exemplars instead of
// checking against them. Run:
//
//	GOWORK=off go test ./internal/protov2 -run Examples -update-examples
var updateExamples = flag.Bool("update-examples", false, "regenerate testdata/examples/*.json")

// exemplar is one canonical message used both as a committed JSON exemplar (the
// PROTOCOL_V2 §13 "examples/" deliverable) and as a protocol test fixture.
type exemplar struct {
	name     string // file name (without .json) and message identity
	value    any    // canonical Go value, marshaled to the exemplar
	incoming bool   // device → server: must round-trip through Decode
}

// exemplars is one entry per v2 message type, covering both directions. The
// incoming ones double as Decode fixtures; all of them double as a drift guard
// on the wire shapes (regenerate with -update-examples and review the diff).
func exemplars() []exemplar {
	return []exemplar{
		// Handshake.
		{"client_hello", ClientHello{
			Type: "hello", ID: 1,
			Client: ClientInfo{Name: "stack-chan", Version: "2.0.0", DeviceID: "aa:bb:cc:dd:ee:ff", UUID: "00000000-0000-0000-0000-000000000000"},
			Audio: AudioConfig{
				In:  AudioStream{Codec: "opus", Rate: 16000, Channels: 1, FrameMS: 60},
				Out: AudioStream{Codec: "opus", Rate: 24000, Channels: 1, FrameMS: 60},
			},
			Features:        []string{"mcp", "wake_word_audio", "camera", "vision_client"},
			TelemetryEvents: []string{"face_seen", "head_touched", "battery_low", "fell_over"},
		}, true},
		{"server_hello", ServerHello{
			Type: "hello", ID: 1, Result: "ok",
			Server: &ServerInfo{Name: "stackchan-server", Version: "2.0.0"},
			Audio: &AudioConfig{
				In:  AudioStream{Rate: 16000, FrameMS: 60},
				Out: AudioStream{Rate: 24000, FrameMS: 60},
			},
			Features:    []string{"mcp", "tools", "vision"},
			Time:        &TimeSync{UnixMS: 1716608284000, TZOffsetMin: -300},
			VisionURL:   "http://192.0.2.10:9099/vision",
			FlowControl: &FlowControl{AudioCreditInitial: 40},
		}, false},
		{"server_hello_error", ServerHello{
			Type: "hello", ID: 1,
			Error: &ErrorBody{Code: "UNSUPPORTED_AUDIO", Message: "Server requires 16k mic input"},
		}, false},

		// Listen lifecycle (device → server).
		{"listen_start", ListenStart{Type: "listen_start", Mode: "auto"}, true},
		{"listen_stop", ListenStop{Type: "listen_stop"}, true},
		{"wake", Wake{Type: "wake", Phrase: "hi_stackchan", Score: 0.81}, true},
		{"abort", Abort{Type: "abort", Reason: "wake"}, true},

		// Speech / audio / display (server → device).
		{"transcript", Transcript{Type: "transcript", Text: "what time is it", Final: true}, false},
		{"audio_begin", AudioBegin{Type: "audio_begin", UtteranceID: 42, EstimatedDurationMS: 4500}, false},
		{"audio_end", AudioEnd{Type: "audio_end", UtteranceID: 42}, false},
		{"audio_cancel", AudioCancel{Type: "audio_cancel", UtteranceID: 42}, false},
		{"caption", Caption{Type: "caption", UtteranceID: 42, Text: "Hi there!", Final: false}, false},
		{"caption_final", Caption{Type: "caption", UtteranceID: 42, Text: "Hi there! How can I help?", Final: true}, false},
		{"display", Display{Type: "display", Emotion: "happy", Status: "speaking"}, false},
		{"alert", Alert{Type: "alert", Title: "Battery low", Message: "Please charge", Emotion: "sad", Sound: "vibration"}, false},
		{"system", System{Type: "system", Command: "reboot"}, false},

		// Flow control (device → server).
		{"audio_credit", AudioCredit{Type: "audio_credit", Frames: 20}, true},

		// Telemetry (device → server).
		{"telemetry", Telemetry{Type: "telemetry", Event: "face_seen", Data: json.RawMessage(`{"x":120,"y":80,"confidence":0.92}`)}, true},

		// Bidirectional.
		{"goodbye", Goodbye{Type: "goodbye", Reason: "user_farewell"}, true},
		{"error", Error{Type: "error", Code: "ASR_TIMEOUT", Message: "ASR service did not respond in 5s", RefID: 42}, true},

		// Tools (both directions).
		{"tool_list_request", ToolList{Type: "tool_list", ID: 5}, true},
		{"tool_list_response", ToolList{Type: "tool_list", ID: 5, Result: &ToolListResult{
			Tools: []ToolDescriptor{{
				Name: "self.audio.set_volume", Description: "Set speaker volume (0-100)",
				ArgsSchema: json.RawMessage(`{"type":"object","properties":{"volume":{"type":"integer","minimum":0,"maximum":100}},"required":["volume"]}`),
				Permission: "public",
			}},
		}}, true},
		{"tool_call_request", ToolCall{Type: "tool_call", ID: 6, Name: "self.audio.set_volume", Args: json.RawMessage(`{"volume":60}`)}, true},
		{"tool_call_response", ToolCall{Type: "tool_call", ID: 6, Result: json.RawMessage(`true`)}, true},
		{"tool_call_error", ToolCall{Type: "tool_call", ID: 6, Error: &ErrorBody{Code: "OUT_OF_RANGE", Message: "volume must be 0..100"}}, true},
	}
}

// TestExamples generates (with -update-examples) or verifies the committed
// JSON exemplars, and confirms every device→server exemplar round-trips through
// Decode to a concrete (non-Unknown) message. This is the §13 examples
// deliverable plus a drift guard on the wire shapes.
func TestExamples(t *testing.T) {
	dir := filepath.Join("testdata", "examples")
	for _, ex := range exemplars() {
		t.Run(ex.name, func(t *testing.T) {
			body, err := json.MarshalIndent(ex.value, "", "  ")
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			body = append(body, '\n')
			path := filepath.Join(dir, ex.name+".json")

			if *updateExamples {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, body, 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			} else {
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read %s: %v (run -update-examples to create)", path, err)
				}
				if string(want) != string(body) {
					t.Errorf("exemplar drift for %s.\n--- got ---\n%s\n--- want ---\n%s\nRun -update-examples if intended.", ex.name, body, want)
				}
			}

			if !ex.incoming {
				return
			}
			msg, err := Decode(body)
			if err != nil {
				t.Fatalf("Decode(%s): %v", ex.name, err)
			}
			if _, isUnknown := msg.(Unknown); isUnknown {
				t.Errorf("Decode(%s) returned Unknown; an incoming exemplar must map to a concrete type", ex.name)
			}
		})
	}
}
