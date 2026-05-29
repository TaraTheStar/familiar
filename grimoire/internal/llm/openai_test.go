// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSSE writes a sequence of OpenAI-style content chunks followed by [DONE].
func fakeSSE(deltas ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, d := range deltas {
			ev := map[string]any{
				"choices": []map[string]any{
					{"delta": map[string]any{"content": d}},
				},
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func TestStreamYieldsContentInOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(fakeSSE("Hello", ", ", "world", "!")))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "test-model", APIKey: "test-key"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var got []string
	for ev, err := range c.Stream(ctx, []Message{{Role: RoleUser, Content: "hi"}}, nil) {
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
		if ev.Content != "" {
			got = append(got, ev.Content)
		}
		if ev.ToolCall != nil {
			t.Errorf("unexpected tool call: %+v", ev.ToolCall)
		}
	}
	want := []string{"Hello", ", ", "world", "!"}
	if len(got) != len(want) {
		t.Fatalf("got %d deltas, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("delta[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestStreamSendsModelAuthAndTools(t *testing.T) {
	var gotBody chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fakeSSE("ok")(w, r)
	}))
	defer srv.Close()

	tools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "self.audio.set_volume",
			Description: "Set the speaker volume",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"volume":{"type":"integer"}}}`),
		},
	}}

	c := &Client{BaseURL: srv.URL, Model: "gemma4-26B-A4B", APIKey: "sk-test"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for range c.Stream(ctx, []Message{
		{Role: RoleSystem, Content: "you are a robot"},
		{Role: RoleUser, Content: "louder"},
	}, tools) {
	}

	if gotBody.Model != "gemma4-26B-A4B" {
		t.Errorf("model=%q", gotBody.Model)
	}
	if !gotBody.Stream {
		t.Error("stream=false; should be true")
	}
	if len(gotBody.Tools) != 1 {
		t.Fatalf("tools len=%d", len(gotBody.Tools))
	}
	if gotBody.Tools[0].Function.Name != "self.audio.set_volume" {
		t.Errorf("tool name=%q", gotBody.Tools[0].Function.Name)
	}
}

func TestStreamSkipsEmptyDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"role":"assistant"}}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"Hi"}}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "data: [DONE]")
		fmt.Fprintln(w)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "x"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var got []string
	for ev, err := range c.Stream(ctx, []Message{{Role: RoleUser, Content: "."}}, nil) {
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ev.Content != "" {
			got = append(got, ev.Content)
		}
	}
	if len(got) != 1 || got[0] != "Hi" {
		t.Errorf("got %v, want [Hi]", got)
	}
}

func TestStreamPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "model gone")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "x"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var sawErr error
	for _, err := range c.Stream(ctx, []Message{{Role: RoleUser, Content: "."}}, nil) {
		if err != nil {
			sawErr = err
			break
		}
	}
	if sawErr == nil || !strings.Contains(sawErr.Error(), "500") {
		t.Errorf("expected 500 error, got %v", sawErr)
	}
}

// TestStreamYieldsToolCall: emulate the multi-chunk OpenAI tool-call
// streaming format. Function name comes first, then JSON arguments
// arrive across several deltas. On finish_reason="tool_calls" we
// expect ONE StreamEvent with the assembled ToolCall.
func TestStreamYieldsToolCall(t *testing.T) {
	chunks := []string{
		// Tool call opens — id + name, empty arguments.
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"self.audio.set_volume","arguments":""}}]}}]}`,
		// Arguments stream in pieces.
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"volu"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"me\":80}"}}]}}]}`,
		// Terminal signal.
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "x"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var calls []ToolCall
	var content string
	for ev, err := range c.Stream(ctx, []Message{{Role: RoleUser, Content: "louder"}}, nil) {
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ev.Content != "" {
			content += ev.Content
		}
		if ev.ToolCall != nil {
			calls = append(calls, *ev.ToolCall)
		}
	}
	if content != "" {
		t.Errorf("unexpected content: %q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("calls=%d, want 1: %+v", len(calls), calls)
	}
	tc := calls[0]
	if tc.ID != "call_abc" || tc.Type != "function" {
		t.Errorf("envelope: %+v", tc)
	}
	if tc.Function.Name != "self.audio.set_volume" {
		t.Errorf("name=%q", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"volume":80}` {
		t.Errorf("arguments=%q", tc.Function.Arguments)
	}
}

// TestStreamYieldsParallelToolCalls: some models emit multiple tool calls
// at once (parallel function calling). Confirm they're all surfaced and
// in the order they arrived.
func TestStreamYieldsParallelToolCalls(t *testing.T) {
	chunks := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"tool_a","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"tool_b","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "x"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var names []string
	for ev, err := range c.Stream(ctx, []Message{{Role: RoleUser, Content: "do two things"}}, nil) {
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ev.ToolCall != nil {
			names = append(names, ev.ToolCall.Function.Name)
		}
	}
	if len(names) != 2 || names[0] != "tool_a" || names[1] != "tool_b" {
		t.Errorf("names=%v, want [tool_a tool_b]", names)
	}
}
