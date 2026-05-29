// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"encoding/json"
	"errors"
	"testing"
)

// Real captures from firmware logs (xiaozhi-esp32 v2.2.4 + dotty patches).

func TestDecodeClientHello(t *testing.T) {
	raw := `{"type":"hello","version":1,"transport":"websocket","features":{"mcp":true},"audio_params":{"format":"opus","sample_rate":16000,"channels":1,"frame_duration":60}}`
	msg, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	h, ok := msg.(ClientHello)
	if !ok {
		t.Fatalf("Decode: got %T, want ClientHello", msg)
	}
	if h.Version != 1 || h.Transport != "websocket" {
		t.Errorf("envelope mismatch: version=%d transport=%q", h.Version, h.Transport)
	}
	if h.Features == nil || !h.Features.MCP {
		t.Errorf("features missing or wrong: %#v", h.Features)
	}
	if h.AudioParams == nil || h.AudioParams.SampleRate != 16000 || h.AudioParams.FrameDuration != 60 {
		t.Errorf("audio_params mismatch: %#v", h.AudioParams)
	}
}

func TestDecodeListenStart(t *testing.T) {
	raw := `{"session_id":"abc","type":"listen","state":"start","mode":"auto"}`
	msg, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	l, ok := msg.(Listen)
	if !ok {
		t.Fatalf("Decode: got %T, want Listen", msg)
	}
	if l.State != "start" || l.Mode != "auto" || l.SessionID != "abc" {
		t.Errorf("Listen fields: %#v", l)
	}
}

func TestDecodeListenDetect(t *testing.T) {
	// wake-word fired; firmware emits this when CONFIG_SEND_WAKE_WORD_DATA=y
	raw := `{"session_id":"abc","type":"listen","state":"detect","text":"Hi,ESP"}`
	msg, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	l := msg.(Listen)
	if l.State != "detect" || l.Text != "Hi,ESP" {
		t.Errorf("Listen detect: %#v", l)
	}
}

func TestDecodeAbort(t *testing.T) {
	raw := `{"session_id":"abc","type":"abort","reason":"wake_word_detected"}`
	msg, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	a := msg.(Abort)
	if a.Reason != "wake_word_detected" {
		t.Errorf("Abort.Reason=%q", a.Reason)
	}
}

func TestDecodeMCPPreservesPayload(t *testing.T) {
	// Device replying to tools/list — payload is opaque JSON-RPC
	raw := `{"session_id":"abc","type":"mcp","payload":{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}}`
	msg, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	m := msg.(MCP)
	// Re-parse the payload; we don't lose any fields.
	var rpc struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
	}
	if err := json.Unmarshal(m.Payload, &rpc); err != nil {
		t.Fatalf("payload reparse: %v", err)
	}
	if rpc.JSONRPC != "2.0" || rpc.ID != 1 {
		t.Errorf("payload contents: %#v", rpc)
	}
}

func TestDecodeEvent(t *testing.T) {
	raw := `{"session_id":"abc","type":"event","name":"face_seen","data":{"x":120,"y":80}}`
	msg, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	e := msg.(Event)
	if e.Name != "face_seen" {
		t.Errorf("Event.Name=%q", e.Name)
	}
}

func TestDecodeUnknownTypeIsNotError(t *testing.T) {
	raw := `{"type":"something_new_in_v3","extra":"ignored"}`
	msg, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	u, ok := msg.(Unknown)
	if !ok {
		t.Fatalf("expected Unknown, got %T", msg)
	}
	if u.Type != "something_new_in_v3" {
		t.Errorf("Unknown.Type=%q", u.Type)
	}
}

func TestDecodeMissingType(t *testing.T) {
	_, err := Decode([]byte(`{"foo":"bar"}`))
	if !errors.Is(err, ErrMissingType) {
		t.Fatalf("expected ErrMissingType, got %v", err)
	}
}

func TestDecodeBadJSON(t *testing.T) {
	_, err := Decode([]byte(`{not json`))
	var ij ErrInvalidJSON
	if !errors.As(err, &ij) {
		t.Fatalf("expected ErrInvalidJSON, got %v", err)
	}
}

// Outgoing messages: we just want to confirm the wire shape matches what the
// firmware parser expects.

func TestEncodeServerHello(t *testing.T) {
	h := ServerHello{
		Type:      "hello",
		Transport: "websocket",
		SessionID: "abc-123",
		AudioParams: &AudioParams{
			SampleRate:    24000,
			FrameDuration: 60,
		},
	}
	out, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"type":"hello","transport":"websocket","session_id":"abc-123","audio_params":{"sample_rate":24000,"frame_duration":60}}`
	if string(out) != want {
		t.Errorf("ServerHello wire shape\ngot:  %s\nwant: %s", out, want)
	}
}

func TestEncodeTTSLifecycle(t *testing.T) {
	cases := []struct {
		tts  TTS
		want string
	}{
		{TTS{Type: "tts", State: "start"}, `{"type":"tts","state":"start"}`},
		{TTS{Type: "tts", State: "sentence_start", Text: "Hi there!"}, `{"type":"tts","state":"sentence_start","text":"Hi there!"}`},
		{TTS{Type: "tts", State: "stop"}, `{"type":"tts","state":"stop"}`},
	}
	for _, c := range cases {
		out, err := json.Marshal(c.tts)
		if err != nil {
			t.Fatalf("Marshal %+v: %v", c.tts, err)
		}
		if string(out) != c.want {
			t.Errorf("TTS %+v\ngot:  %s\nwant: %s", c.tts, out, c.want)
		}
	}
}

func TestEncodeSTTAndLLM(t *testing.T) {
	stt, _ := json.Marshal(STT{Type: "stt", Text: "what time is it"})
	if string(stt) != `{"type":"stt","text":"what time is it"}` {
		t.Errorf("STT: %s", stt)
	}
	llm, _ := json.Marshal(LLM{Type: "llm", Emotion: "happy"})
	if string(llm) != `{"type":"llm","emotion":"happy"}` {
		t.Errorf("LLM: %s", llm)
	}
}

func TestEncodeOTAResponse(t *testing.T) {
	resp := OTAResponse{
		WebSocket:  &WebSocketConfig{URL: "ws://192.0.2.10:9098/xiaozhi/v1/"},
		ServerTime: &ServerTime{Timestamp: 1716608284000, TimezoneOffset: -300},
		Firmware:   &Firmware{Version: "1.3.1", URL: "http://example/x.bin"},
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Just spot-check the shape — order is stable for omitempty-free fields.
	var roundtrip OTAResponse
	if err := json.Unmarshal(out, &roundtrip); err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	if roundtrip.WebSocket.URL != resp.WebSocket.URL {
		t.Errorf("URL roundtrip mismatch")
	}
	if roundtrip.ServerTime.TimezoneOffset != -300 {
		t.Errorf("TZ roundtrip mismatch")
	}
}
