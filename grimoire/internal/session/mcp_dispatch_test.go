// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
)

// fakeProvider is a ToolProvider stand-in (no real MCP) that records calls, so
// the session-side wiring (snapshotTools exposure + dispatchToolCall routing)
// can be tested without the SDK or a subprocess. The real adapter is covered in
// internal/mcptools.
type fakeProvider struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeProvider) Tools() []llm.Tool {
	return []llm.Tool{{
		Type:     "function",
		Function: llm.ToolFunction{Name: "mcp__fetch__fetch", Description: "fetch a url"},
	}}
}

func (f *fakeProvider) Handles(name string) bool { return name == "mcp__fetch__fetch" }

func (f *fakeProvider) Call(_ context.Context, name string, args json.RawMessage) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+string(args))
	f.mu.Unlock()
	return "weather: sunny", nil
}

func discardSession(cfg Config) *Session {
	return &Session{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestServerToolsInSnapshot proves a ToolProvider's tools are exposed to the LLM
// (appended to the per-turn snapshot), so an external MCP tool is callable.
func TestServerToolsInSnapshot(t *testing.T) {
	s := discardSession(Config{ServerTools: &fakeProvider{}})
	tools := s.snapshotTools()
	for _, tl := range tools {
		if tl.Function.Name == "mcp__fetch__fetch" {
			return
		}
	}
	t.Fatalf("server tool mcp__fetch__fetch not in snapshot: %+v", tools)
}

// TestDispatchRoutesToServerTool proves dispatchToolCall routes a provider tool
// to the provider (in-process) and returns its result, never the device port.
func TestDispatchRoutesToServerTool(t *testing.T) {
	fp := &fakeProvider{}
	s := discardSession(Config{ServerTools: fp})

	res, err := s.dispatchToolCall(context.Background(), llm.ToolCall{
		ID:       "1",
		Type:     "function",
		Function: llm.ToolCallFunction{Name: "mcp__fetch__fetch", Arguments: `{"url":"x"}`},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res != "weather: sunny" {
		t.Errorf("result = %q, want %q", res, "weather: sunny")
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.calls) != 1 || fp.calls[0] != `mcp__fetch__fetch {"url":"x"}` {
		t.Errorf("provider calls = %v, want one call with args", fp.calls)
	}
}
