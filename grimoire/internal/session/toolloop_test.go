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
	"github.com/TaraTheStar/familiar/grimoire/internal/mcp"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// TestFullToolLoop exercises the MCP-enabled voice loop:
//
//  1. Device connects, hello roundtrip succeeds.
//  2. Server runs MCP initialize → device replies serverInfo.
//  3. Server runs tools/list → device returns one tool (set_volume).
//  4. Device sends listen:start, audio, listen:stop.
//  5. Server transcribes (fake ASR) → "set volume to 60".
//  6. Server calls LLM (mock) with tools — LLM emits a tool_call.
//  7. Server dispatches tool_call to device via MCP.
//  8. Device returns "volume set" as tool result.
//  9. Server calls LLM again — LLM emits final content "Volume set to 60.".
//  10. Server speaks the content.
//
// Asserts the conversation flow on both sides + the on-wire shape.
func TestFullToolLoop(t *testing.T) {
	llmCalls := &atomic.Int32{}

	// Mock LLM server. First call returns tool_call; second returns content.
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := llmCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")

		// Decode the request to confirm tools were passed + dialogue grew.
		var body struct {
			Messages []json.RawMessage `json:"messages"`
			Tools    []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Tools) == 0 {
			t.Errorf("LLM call %d: no tools sent to LLM", call)
		}

		write := func(s string) {
			fmt.Fprintf(w, "data: %s\n\n", s)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}

		if call == 1 {
			// Must have exactly the user message at this point.
			if len(body.Messages) < 1 {
				t.Errorf("call 1: expected at least 1 message")
			}
			// Emit a tool_call in two chunks then terminal.
			write(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"self.audio.set_volume","arguments":""}}]}}]}`)
			write(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"volume\":60}"}}]}}]}`)
			write(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
			write("[DONE]")
			return
		}

		// Second call should include the tool result.
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
		// Final text response.
		write(`{"choices":[{"delta":{"content":"Volume set to 60."}}]}`)
		write(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		write("[DONE]")
	}))
	defer llmServer.Close()

	// Mock Kokoro (returns 60ms of zero PCM).
	kokoro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(make([]byte, 2880)) // 1 Opus frame's worth
	}))
	defer kokoro.Close()

	// Spin up the stackend.
	srv := httptest.NewServer(Handler(Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
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

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Client hello.
	mustWriteText(t, ctx, conn, mustJSON(t, protocol.ClientHello{
		Type: "hello", Version: 1, Transport: "websocket",
		AudioParams: &protocol.AudioParams{Format: "opus", SampleRate: 16000, Channels: 1, FrameDuration: 60},
	}))
	mustReadText(t, ctx, conn) // server hello

	// Helpers to handle MCP frames coming from the server.
	expectAndReplyMCP := func(expectedMethod string, result any) {
		t.Helper()
		mt, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read mcp %s: %v", expectedMethod, err)
		}
		if mt != websocket.MessageText {
			t.Fatalf("expected text frame for %s, got binary", expectedMethod)
		}
		var env struct {
			SessionID string          `json:"session_id"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("decode mcp envelope: %v", err)
		}
		if env.Type != "mcp" {
			t.Fatalf("expected type=mcp for %s, got type=%s (raw=%s)", expectedMethod, env.Type, data)
		}
		var req mcp.Request
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			t.Fatalf("decode mcp request: %v", err)
		}
		if req.Method != expectedMethod {
			t.Fatalf("expected method=%s, got %s", expectedMethod, req.Method)
		}

		// Build the JSON-RPC response.
		resultBytes, _ := json.Marshal(result)
		resp := mcp.Response{JSONRPC: "2.0", ID: req.ID, Result: resultBytes}
		respBytes, _ := json.Marshal(resp)

		mustWriteText(t, ctx, conn, mustJSON(t, protocol.MCP{
			SessionID: env.SessionID,
			Type:      "mcp",
			Payload:   respBytes,
		}))
	}

	// MCP initialize + tools/list.
	expectAndReplyMCP("initialize", map[string]any{
		"protocolVersion": mcp.MCPVersion,
		"serverInfo":      map[string]string{"name": "fake-device", "version": "1.0"},
	})
	expectAndReplyMCP("tools/list", mcp.ListToolsResult{
		Tools: []mcp.ToolDescriptor{{
			Name:        "self.audio.set_volume",
			Description: "Set the audio volume",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"volume":{"type":"integer"}},"required":["volume"]}`),
		}},
	})

	// Send the user turn.
	mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "start", Mode: "auto"}))
	sendFakeAudioFrame(t, ctx, conn)
	mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "stop"}))

	// Now we expect, in some order:
	//   stt frame (right after ASR)
	//   mcp tools/call request → we reply with "ok"
	//   tts:start, sentence_start("Volume set to 60."), N opus frames, tts:stop
	sttSeen := false
	toolCallSeen := false
	gotSentence := ""
	deadline := time.Now().Add(8 * time.Second)

	for time.Now().Before(deadline) && gotSentence == "" {
		mt, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if mt == websocket.MessageBinary {
			continue
		}
		var head struct {
			Type, State, Text string
			Payload           json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			t.Fatalf("decode: %v", err)
		}
		switch head.Type {
		case "stt":
			if head.Text != "set volume to 60" {
				t.Errorf("stt text=%q", head.Text)
			}
			sttSeen = true
		case "mcp":
			// Tool call from server.
			var req mcp.Request
			if err := json.Unmarshal(head.Payload, &req); err != nil {
				t.Fatalf("decode mcp tool call: %v", err)
			}
			if req.Method != "tools/call" {
				t.Fatalf("expected tools/call, got %s", req.Method)
			}
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.Name != "self.audio.set_volume" {
				t.Errorf("tool call name=%q", p.Name)
			}
			if string(p.Arguments) != `{"volume":60}` {
				t.Errorf("tool call args=%q", p.Arguments)
			}
			toolCallSeen = true

			// Reply with the tool result.
			result := mcp.CallToolResult{
				Content: []mcp.ContentBlock{{Type: "text", Text: "volume set"}},
			}
			rawResult, _ := json.Marshal(result)
			resp := mcp.Response{JSONRPC: "2.0", ID: req.ID, Result: rawResult}
			respBytes, _ := json.Marshal(resp)
			mustWriteText(t, ctx, conn, mustJSON(t, protocol.MCP{
				Type:    "mcp",
				Payload: respBytes,
			}))
		case "tts":
			if head.State == "sentence_start" {
				gotSentence = head.Text
			}
		}
	}

	if !sttSeen {
		t.Error("never saw stt frame")
	}
	if !toolCallSeen {
		t.Error("never saw tool call from server")
	}
	if gotSentence != "Volume set to 60." {
		t.Errorf("sentence=%q, want %q", gotSentence, "Volume set to 60.")
	}
	if llmCalls.Load() != 2 {
		t.Errorf("LLM called %d times, want 2 (tool → content)", llmCalls.Load())
	}
}
