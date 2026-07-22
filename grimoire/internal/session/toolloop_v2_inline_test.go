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

	"github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// TestToolLoopV2Inline proves the tools_inline fast path (PROTOCOL_V2 §6.4): a
// device that announces its tool catalog in the hello lets the server skip the
// tool_list round-trip entirely. Same tool-using turn as TestFullToolLoopV2,
// but the catalog rides the hello and the server must issue NO tool_list.
func TestToolLoopV2Inline(t *testing.T) {
	llmCalls := &atomic.Int32{}
	llmServer := inlineToolLoopLLM(t, llmCalls)
	defer llmServer.Close()
	kokoro := inlineKokoroStub(t)
	defer kokoro.Close()

	srv := httptest.NewServer(Handler(Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, Channels: 1, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		MCPInitTimeout:   2 * time.Second,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
		ASR:              &fakeASR{out: "set volume to 60"},
		LLM:              &llm.OpenAIClient{Endpoint: llmServer.URL, Model: "test"},
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

	// Hello announces the catalog inline — no tool_list exchange should follow.
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ClientHello{
		Type: "hello", ID: 1,
		Client: protov2.ClientInfo{
			Name:        "stack-chan",
			ToolsInline: []protov2.ToolDescriptor{setVolumeDescriptor()},
		},
		Audio: protov2.AudioConfig{
			In:  protov2.AudioStream{Codec: "opus", Rate: 16000, Channels: 1, FrameMS: 60},
			Out: protov2.AudioStream{Codec: "opus", Rate: 24000, Channels: 1, FrameMS: 60},
		},
		Features: []string{"tools"},
	}))
	mustReadText(t, ctx, conn) // server hello

	// Drive the turn immediately (no tool_list to answer first).
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStart{Type: "listen_start", Mode: "auto"}))
	sendFakeAudioFrame(t, ctx, conn)
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStop{Type: "listen_stop"}))

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
		case "tool_list":
			// The whole point of tools_inline: this must never arrive. Reply so
			// the server doesn't stall, but the test has already failed.
			t.Error("server issued tool_list despite tools_inline (fast path not taken)")
			var tl protov2.ToolList
			_ = json.Unmarshal(data, &tl)
			mustWriteText(t, ctx, conn, mustJSON(t, protov2.ToolList{
				Type: "tool_list", ID: tl.ID,
				Result: &protov2.ToolListResult{Tools: []protov2.ToolDescriptor{setVolumeDescriptor()}},
			}))
		case "transcript":
			transcriptSeen = true
		case "tool_call":
			var tc protov2.ToolCall
			_ = json.Unmarshal(data, &tc)
			if tc.Name != "self.audio.set_volume" {
				t.Errorf("tool_call name = %q", tc.Name)
			}
			toolCallSeen = true
			mustWriteText(t, ctx, conn, mustJSON(t, protov2.ToolCall{
				Type: "tool_call", ID: tc.ID, Result: json.RawMessage(`true`),
			}))
		case "caption":
			var cap protov2.Caption
			_ = json.Unmarshal(data, &cap)
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
		t.Error("never saw first-class tool_call from server (inline tool not registered)")
	}
	if gotCaption != "Volume set to 60." {
		t.Errorf("final caption = %q, want %q", gotCaption, "Volume set to 60.")
	}
	if llmCalls.Load() != 2 {
		t.Errorf("LLM called %d times, want 2 (tool → content)", llmCalls.Load())
	}
}

// TestToolLoopV2InlineFallback proves the belt-and-suspenders fallback: an
// inline catalog that is present but has no usable (named) descriptor must NOT
// leave the session toolless — the server falls back to tool_list discovery.
func TestToolLoopV2InlineFallback(t *testing.T) {
	llmCalls := &atomic.Int32{}
	llmServer := inlineToolLoopLLM(t, llmCalls)
	defer llmServer.Close()
	kokoro := inlineKokoroStub(t)
	defer kokoro.Close()

	srv := httptest.NewServer(Handler(Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, Channels: 1, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		MCPInitTimeout:   2 * time.Second,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
		ASR:              &fakeASR{out: "set volume to 60"},
		LLM:              &llm.OpenAIClient{Endpoint: llmServer.URL, Model: "test"},
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

	// Inline is present but unusable (a nameless descriptor) → server must fall
	// back to tool_list rather than run toolless.
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ClientHello{
		Type: "hello", ID: 1,
		Client: protov2.ClientInfo{
			Name: "stack-chan",
			ToolsInline: []protov2.ToolDescriptor{{
				Description: "malformed: no name",
				Permission:  "public",
			}},
		},
		Audio: protov2.AudioConfig{
			In:  protov2.AudioStream{Codec: "opus", Rate: 16000, Channels: 1, FrameMS: 60},
			Out: protov2.AudioStream{Codec: "opus", Rate: 24000, Channels: 1, FrameMS: 60},
		},
		Features: []string{"tools"},
	}))
	mustReadText(t, ctx, conn) // server hello

	// Fallback: the server's next frame is the tool_list it fell back to.
	tlReq := readToolFrame(t, ctx, conn)
	tl, ok := tlReq.(protov2.ToolList)
	if !ok {
		t.Fatalf("expected tool_list fallback, got %T", tlReq)
	}
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ToolList{
		Type: "tool_list", ID: tl.ID,
		Result: &protov2.ToolListResult{Tools: []protov2.ToolDescriptor{setVolumeDescriptor()}},
	}))

	// Turn proceeds normally; the recovered tool is callable.
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStart{Type: "listen_start", Mode: "auto"}))
	sendFakeAudioFrame(t, ctx, conn)
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStop{Type: "listen_stop"}))

	toolCallSeen := false
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
		case "tool_call":
			var tc protov2.ToolCall
			_ = json.Unmarshal(data, &tc)
			toolCallSeen = true
			mustWriteText(t, ctx, conn, mustJSON(t, protov2.ToolCall{
				Type: "tool_call", ID: tc.ID, Result: json.RawMessage(`true`),
			}))
		case "caption":
			var cap protov2.Caption
			_ = json.Unmarshal(data, &cap)
			if cap.Final {
				captionDone = true
			}
		}
	}
	if !toolCallSeen {
		t.Error("never saw tool_call after fallback (recovered tool not registered)")
	}
}

// setVolumeDescriptor is the one device tool used by the inline tool-loop tests.
func setVolumeDescriptor() protov2.ToolDescriptor {
	return protov2.ToolDescriptor{
		Name:        "self.audio.set_volume",
		Description: "Set the audio volume",
		ArgsSchema:  json.RawMessage(`{"type":"object","properties":{"volume":{"type":"integer"}},"required":["volume"]}`),
		Permission:  "public",
	}
}

// inlineToolLoopLLM is the two-call LLM stub shared by the inline tests: call 1
// emits a set_volume tool_call, call 2 (which must see the tool result) emits
// the final content. It also asserts the LLM always receives a non-empty tool
// list, proving the catalog reached it however it was discovered.
func inlineToolLoopLLM(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")

		var body struct {
			Messages []json.RawMessage `json:"messages"`
			Tools    []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Tools) == 0 {
			t.Errorf("LLM call %d: no tools sent to LLM (catalog did not populate)", call)
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
		write(`{"choices":[{"delta":{"content":"Volume set to 60."}}]}`)
		write(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		write("[DONE]")
	}))
}

// inlineKokoroStub returns enough PCM for a couple of Opus frames.
func inlineKokoroStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(make([]byte, 2880))
	}))
}
