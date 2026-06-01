// SPDX-License-Identifier: AGPL-3.0-or-later

package mcptools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testManager builds a Manager wired to an in-process MCP server over the SDK's
// in-memory transport, so the adapter is exercised against a real MCP peer
// without spawning a subprocess. The server exposes a single "ping" tool that
// echoes back "pong: <msg>" so the test can assert argument pass-through too.
func testManager(t *testing.T) *Manager {
	t.Helper()
	serverT, clientT := mcp.NewInMemoryTransports()

	srv := mcp.NewServer(&mcp.Implementation{Name: "test-mcp", Version: "1.0.0"}, nil)
	srv.AddTool(
		&mcp.Tool{
			Name:        "ping",
			Description: "returns pong",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}}}`),
		},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct {
				Msg string `json:"msg"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &args)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "pong: " + args.Msg}},
			}, nil
		},
	)

	ctx := context.Background()
	go func() { _ = srv.Run(ctx, serverT) }()

	m := &Manager{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		handlers: make(map[string]serverTool),
	}
	if err := m.connect(ctx, "test", clientT); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestManagerListsAndNamespacesTools(t *testing.T) {
	m := testManager(t)

	tools := m.Tools()
	if len(tools) != 1 {
		t.Fatalf("Tools() = %d tools, want 1", len(tools))
	}
	const want = "mcp__test__ping"
	if got := tools[0].Function.Name; got != want {
		t.Errorf("tool name = %q, want %q (namespaced)", got, want)
	}
	if tools[0].Function.Description != "returns pong" {
		t.Errorf("description not carried: %q", tools[0].Function.Description)
	}
	if !m.Handles(want) {
		t.Errorf("Handles(%q) = false, want true", want)
	}
	if m.Handles("ping") {
		t.Errorf("Handles(%q) = true; un-namespaced name must not match", "ping")
	}
}

func TestManagerCallRoutesToServer(t *testing.T) {
	m := testManager(t)

	got, err := m.Call(context.Background(), "mcp__test__ping", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "pong: hi" {
		t.Errorf("Call result = %q, want %q (args passed through, text flattened)", got, "pong: hi")
	}

	if _, err := m.Call(context.Background(), "mcp__test__nope", nil); err == nil {
		t.Errorf("Call on unknown tool: want error, got nil")
	}
}
