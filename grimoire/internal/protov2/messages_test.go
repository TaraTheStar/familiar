// SPDX-License-Identifier: AGPL-3.0-or-later

package protov2

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodeClientHello(t *testing.T) {
	raw := `{"type":"hello","id":1,
		"client":{"name":"stack-chan","version":"2.0.0","device_id":"aa:bb:cc:dd:ee:ff","uuid":"00000000-0000-0000-0000-000000000000"},
		"audio":{"in":{"codec":"opus","rate":16000,"channels":1,"frame_ms":60},"out":{"codec":"opus","rate":24000,"channels":1,"frame_ms":60}},
		"features":["mcp","wake_word_audio"],
		"telemetry_events":["face_seen","battery_low"]}`
	msg, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	h, ok := msg.(ClientHello)
	if !ok {
		t.Fatalf("Decode: got %T, want ClientHello", msg)
	}
	if h.ID != 1 || h.Client.Name != "stack-chan" {
		t.Errorf("envelope mismatch: id=%d client=%q", h.ID, h.Client.Name)
	}
	if h.Audio.In.Rate != 16000 || h.Audio.Out.Rate != 24000 {
		t.Errorf("audio mismatch: in=%d out=%d", h.Audio.In.Rate, h.Audio.Out.Rate)
	}
	if len(h.Features) != 2 || h.Features[1] != "wake_word_audio" {
		t.Errorf("features mismatch: %#v", h.Features)
	}
}

func TestDecodeListenSplit(t *testing.T) {
	// v2's headline fix: the overloaded v1 "listen" is three distinct types.
	cases := []struct {
		raw  string
		want any
	}{
		{`{"type":"listen_start","mode":"auto"}`, ListenStart{Type: "listen_start", Mode: "auto"}},
		{`{"type":"listen_stop"}`, ListenStop{Type: "listen_stop"}},
		{`{"type":"wake","phrase":"hi_stackchan","score":0.81}`, Wake{Type: "wake", Phrase: "hi_stackchan", Score: 0.81}},
	}
	for _, c := range cases {
		msg, err := Decode([]byte(c.raw))
		if err != nil {
			t.Fatalf("Decode(%s): %v", c.raw, err)
		}
		if msg != c.want {
			t.Errorf("Decode(%s) = %#v, want %#v", c.raw, msg, c.want)
		}
	}
}

func TestDecodeAudioCredit(t *testing.T) {
	msg, err := Decode([]byte(`{"type":"audio_credit","frames":20}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c, ok := msg.(AudioCredit)
	if !ok || c.Frames != 20 {
		t.Fatalf("got %#v, want AudioCredit{Frames:20}", msg)
	}
}

func TestDecodeTelemetryPreservesData(t *testing.T) {
	raw := `{"type":"telemetry","event":"face_seen","data":{"x":120,"y":80,"confidence":0.92}}`
	msg, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	tel := msg.(Telemetry)
	if tel.Event != "face_seen" {
		t.Errorf("event=%q", tel.Event)
	}
	var data struct{ X, Y int }
	if err := json.Unmarshal(tel.Data, &data); err != nil {
		t.Fatalf("data reparse: %v", err)
	}
	if data.X != 120 || data.Y != 80 {
		t.Errorf("data contents: %#v", data)
	}
}

func TestDecodeToolCallResponse(t *testing.T) {
	// Device's reply to a server tool_call: result is opaque JSON.
	raw := `{"type":"tool_call","id":6,"result":{"volume":60,"muted":false}}`
	msg, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	tc := msg.(ToolCall)
	if tc.ID != 6 || tc.Error != nil {
		t.Errorf("tool_call: %#v", tc)
	}
	var res struct{ Volume int }
	if err := json.Unmarshal(tc.Result, &res); err != nil {
		t.Fatalf("result reparse: %v", err)
	}
	if res.Volume != 60 {
		t.Errorf("result.Volume=%d", res.Volume)
	}
}

func TestDecodeToolCallError(t *testing.T) {
	raw := `{"type":"tool_call","id":6,"error":{"code":"OUT_OF_RANGE","message":"volume must be 0..100"}}`
	msg, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	tc := msg.(ToolCall)
	if tc.Error == nil || tc.Error.Code != "OUT_OF_RANGE" {
		t.Errorf("expected nested error, got %#v", tc)
	}
	if tc.Result != nil {
		t.Errorf("error response should have no result: %s", tc.Result)
	}
}

func TestDecodeGoodbyeAndError(t *testing.T) {
	g, err := Decode([]byte(`{"type":"goodbye","reason":"user_farewell"}`))
	if err != nil {
		t.Fatalf("Decode goodbye: %v", err)
	}
	if g.(Goodbye).Reason != "user_farewell" {
		t.Errorf("goodbye: %#v", g)
	}
	e, err := Decode([]byte(`{"type":"error","code":"ASR_TIMEOUT","message":"slow","ref_id":42}`))
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	em := e.(Error)
	if em.Code != "ASR_TIMEOUT" || em.RefID != 42 {
		t.Errorf("error: %#v", em)
	}
}

func TestDecodeUnknownTypeIsNotError(t *testing.T) {
	msg, err := Decode([]byte(`{"type":"something_new_in_v3","extra":"ignored"}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	u, ok := msg.(Unknown)
	if !ok || u.Type != "something_new_in_v3" {
		t.Fatalf("expected Unknown{something_new_in_v3}, got %#v", msg)
	}
}

func TestDecodeMissingType(t *testing.T) {
	if _, err := Decode([]byte(`{"foo":"bar"}`)); !errors.Is(err, ErrMissingType) {
		t.Fatalf("expected ErrMissingType, got %v", err)
	}
}

func TestDecodeBadJSON(t *testing.T) {
	var ij ErrInvalidJSON
	if _, err := Decode([]byte(`{not json`)); !errors.As(err, &ij) {
		t.Fatalf("expected ErrInvalidJSON, got %v", err)
	}
}

// Outgoing wire shapes — confirm the JSON matches what the spec documents and
// the firmware parser will expect.

func TestEncodeServerHello(t *testing.T) {
	h := ServerHello{
		Type:   "hello",
		ID:     1,
		Result: "ok",
		Server: &ServerInfo{Name: "stackchan-server", Version: "2.0.0"},
		Audio: &AudioConfig{
			In:  AudioStream{Rate: 16000, FrameMS: 60},
			Out: AudioStream{Rate: 24000, FrameMS: 60},
		},
		FlowControl: &FlowControl{AudioCreditInitial: 40},
	}
	out, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"type":"hello","id":1,"result":"ok","server":{"name":"stackchan-server","version":"2.0.0"},"audio":{"in":{"rate":16000,"frame_ms":60},"out":{"rate":24000,"frame_ms":60}},"flow_control":{"audio_credit_initial":40}}`
	if string(out) != want {
		t.Errorf("ServerHello wire shape\ngot:  %s\nwant: %s", out, want)
	}
}

func TestEncodeAudioAndCaptionLifecycle(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{AudioBegin{Type: "audio_begin", UtteranceID: 7}, `{"type":"audio_begin","utterance_id":7}`},
		{Caption{Type: "caption", UtteranceID: 7, Text: "Hi there!", Final: false}, `{"type":"caption","utterance_id":7,"text":"Hi there!","final":false}`},
		{Caption{Type: "caption", UtteranceID: 7, Text: "Hi there! How can I help?", Final: true}, `{"type":"caption","utterance_id":7,"text":"Hi there! How can I help?","final":true}`},
		{AudioEnd{Type: "audio_end", UtteranceID: 7}, `{"type":"audio_end","utterance_id":7}`},
	}
	for _, c := range cases {
		out, err := json.Marshal(c.v)
		if err != nil {
			t.Fatalf("Marshal %+v: %v", c.v, err)
		}
		if string(out) != c.want {
			t.Errorf("%T\ngot:  %s\nwant: %s", c.v, out, c.want)
		}
	}
}

func TestEncodeTranscriptAndDisplay(t *testing.T) {
	tr, _ := json.Marshal(Transcript{Type: "transcript", Text: "what time is it", Final: true})
	if string(tr) != `{"type":"transcript","text":"what time is it","final":true}` {
		t.Errorf("Transcript: %s", tr)
	}
	d, _ := json.Marshal(Display{Type: "display", Emotion: "happy", Status: "speaking"})
	if string(d) != `{"type":"display","emotion":"happy","status":"speaking"}` {
		t.Errorf("Display: %s", d)
	}
	// status alone (independently optional, §4.5)
	s, _ := json.Marshal(Display{Type: "display", Status: "listening"})
	if string(s) != `{"type":"display","status":"listening"}` {
		t.Errorf("Display status-only: %s", s)
	}
}

func TestEncodeToolCallRequest(t *testing.T) {
	out, err := json.Marshal(ToolCall{Type: "tool_call", ID: 6, Name: "self.audio.set_volume", Args: json.RawMessage(`{"volume":60}`)})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"type":"tool_call","id":6,"name":"self.audio.set_volume","args":{"volume":60}}`
	if string(out) != want {
		t.Errorf("ToolCall request\ngot:  %s\nwant: %s", out, want)
	}
}
