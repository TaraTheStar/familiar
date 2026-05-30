// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// TestFullToolLoopV2 is the Protocol v2 analogue of TestFullToolLoop: it drives
// the same tool-using turn through a v2 session and a fake v2 device, proving
// the first-class tool path end to end:
//
//  1. v2 hello roundtrip.
//  2. Server discovers tools via tool_list (no JSON-RPC envelope) → device
//     returns one tool (set_volume).
//  3. Device sends listen_start, audio, listen_stop.
//  4. Server transcribes (fake ASR) → "set volume to 60".
//  5. LLM emits a tool_call; server dispatches it as a first-class tool_call.
//  6. Device replies tool_call {result:true}; toolResultText → "ok".
//  7. LLM emits final content; server speaks it (audio_begin → caption → frames
//     → audio_end).
func TestFullToolLoopV2(t *testing.T) {
	llmCalls := &atomic.Int32{}

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := llmCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")

		var body struct {
			Messages []json.RawMessage `json:"messages"`
			Tools    []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Tools) == 0 {
			t.Errorf("LLM call %d: no tools sent to LLM (v2 discovery did not populate)", call)
		}

		write := func(s string) {
			fmt.Fprintf(w, "data: %s\n\n", s)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}

		if call == 1 {
			write(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"self.audio.set_volume","arguments":""}}]}}]}`)
			write(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"volume\":60}"}}]}}]}`)
			write(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
			write("[DONE]")
			return
		}
		hasToolMsg := false
		for _, m := range body.Messages {
			var msg struct {
				Role string `json:"role"`
			}
			_ = json.Unmarshal(m, &msg)
			if msg.Role == "tool" {
				hasToolMsg = true
			}
		}
		if !hasToolMsg {
			t.Errorf("call 2: dialogue is missing the tool result message")
		}
		write(`{"choices":[{"delta":{"content":"Volume set to 60."}}]}`)
		write(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		write("[DONE]")
	}))
	defer llmServer.Close()

	kokoro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(make([]byte, 2880)) // 2 Opus frames' worth
	}))
	defer kokoro.Close()

	srv := httptest.NewServer(Handler(Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, Channels: 1, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		MCPInitTimeout:   2 * time.Second,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
		ASR:              &fakeASR{out: "set volume to 60"},
		LLM:              &llm.Client{BaseURL: llmServer.URL, Model: "test"},
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Protocol-Version": []string{"2"}},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// v2 client hello → read server hello.
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ClientHello{
		Type: "hello", ID: 1,
		Client: protov2.ClientInfo{Name: "stack-chan"},
		Audio: protov2.AudioConfig{
			In:  protov2.AudioStream{Codec: "opus", Rate: 16000, Channels: 1, FrameMS: 60},
			Out: protov2.AudioStream{Codec: "opus", Rate: 24000, Channels: 1, FrameMS: 60},
		},
		Features: []string{"tools"},
	}))
	mustReadText(t, ctx, conn) // server hello

	// Discovery: the server's first post-hello frame is tool_list. Reply with
	// the catalog so the LLM call sees the tool.
	tlReq := readToolFrame(t, ctx, conn)
	tl, ok := tlReq.(protov2.ToolList)
	if !ok {
		t.Fatalf("expected tool_list discovery, got %T", tlReq)
	}
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ToolList{
		Type: "tool_list", ID: tl.ID,
		Result: &protov2.ToolListResult{Tools: []protov2.ToolDescriptor{{
			Name:        "self.audio.set_volume",
			Description: "Set the audio volume",
			ArgsSchema:  json.RawMessage(`{"type":"object","properties":{"volume":{"type":"integer"}},"required":["volume"]}`),
			Permission:  "public",
		}}},
	}))

	// Drive the turn.
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStart{Type: "listen_start", Mode: "auto"}))
	sendFakeAudioFrame(t, ctx, conn)
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStop{Type: "listen_stop"}))

	// Expect: transcript, a tool_call (we reply true), captions, audio frames.
	transcriptSeen := false
	toolCallSeen := false
	gotCaption := ""
	captionDone := false
	deadline := time.Now().Add(8 * time.Second)

	for time.Now().Before(deadline) && !captionDone {
		mt, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if mt == websocket.MessageBinary {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			t.Fatalf("decode: %v", err)
		}
		switch head.Type {
		case "transcript":
			var tr protov2.Transcript
			_ = json.Unmarshal(data, &tr)
			if tr.Text != "set volume to 60" || !tr.Final {
				t.Errorf("transcript = %+v", tr)
			}
			transcriptSeen = true
		case "tool_call":
			var tc protov2.ToolCall
			_ = json.Unmarshal(data, &tc)
			if tc.Name != "self.audio.set_volume" {
				t.Errorf("tool_call name = %q", tc.Name)
			}
			if string(tc.Args) != `{"volume":60}` {
				t.Errorf("tool_call args = %q", tc.Args)
			}
			toolCallSeen = true
			mustWriteText(t, ctx, conn, mustJSON(t, protov2.ToolCall{
				Type: "tool_call", ID: tc.ID, Result: json.RawMessage(`true`),
			}))
		case "caption":
			var cap protov2.Caption
			_ = json.Unmarshal(data, &cap)
			// Cumulative text rides the non-terminal captions; the terminal one
			// (Final=true) carries no text and just marks completion (§4.4).
			if cap.Text != "" {
				gotCaption = cap.Text
			}
			if cap.Final {
				captionDone = true
			}
		}
	}

	if !transcriptSeen {
		t.Error("never saw transcript frame")
	}
	if !toolCallSeen {
		t.Error("never saw first-class tool_call from server")
	}
	if gotCaption != "Volume set to 60." {
		t.Errorf("final caption = %q, want %q", gotCaption, "Volume set to 60.")
	}
	if llmCalls.Load() != 2 {
		t.Errorf("LLM called %d times, want 2 (tool → content)", llmCalls.Load())
	}
}

// readToolFrame reads text frames until a tool_list/tool_call arrives, returning
// the decoded message. Other frames (e.g. an early display) are skipped.
func readToolFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) protov2.Message {
	t.Helper()
	for {
		mt, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read tool frame: %v", err)
		}
		if mt != websocket.MessageText {
			continue
		}
		msg, err := protov2.Decode(data)
		if err != nil {
			t.Fatalf("decode tool frame: %v", err)
		}
		switch msg.(type) {
		case protov2.ToolList, protov2.ToolCall:
			return msg
		}
	}
}
