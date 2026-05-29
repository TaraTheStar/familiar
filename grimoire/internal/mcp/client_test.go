// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDevice is an in-process MCP "device" that just replies to requests
// via the Client.HandleIncoming back-channel. Lets us test the client
// end-to-end without a real WS.
type fakeDevice struct {
	mu        sync.Mutex
	c         *Client                               // set after wiring
	responses map[string]func(req Request) Response // method → response builder
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{responses: map[string]func(Request) Response{}}
}

func (d *fakeDevice) on(method string, fn func(Request) Response) { d.responses[method] = fn }

// send is the Sender the Client uses; we receive the request, build a
// response based on the registered handler, and feed it back via
// HandleIncoming on a goroutine so the client doesn't deadlock.
func (d *fakeDevice) send(_ context.Context, payload []byte) error {
	var req Request
	if err := json.Unmarshal(payload, &req); err != nil {
		return err
	}
	fn, ok := d.responses[req.Method]
	if !ok {
		// Unknown method → JSON-RPC error response.
		resp := Response{JSONRPC: "2.0", ID: req.ID,
			Error: &RPCError{Code: -32601, Message: "method not found: " + req.Method}}
		go d.reply(resp)
		return nil
	}
	resp := fn(req)
	resp.JSONRPC = "2.0"
	resp.ID = req.ID
	go d.reply(resp)
	return nil
}

func (d *fakeDevice) reply(resp Response) {
	b, _ := json.Marshal(resp)
	d.c.HandleIncoming(b)
}

func TestInitializeRoundtrip(t *testing.T) {
	dev := newFakeDevice()
	dev.on("initialize", func(_ Request) Response {
		raw, _ := json.Marshal(map[string]any{
			"protocolVersion": MCPVersion,
			"serverInfo":      map[string]string{"name": "test-device", "version": "1.0"},
		})
		return Response{Result: raw}
	})
	c := NewClient(dev.send)
	dev.c = c

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	raw, err := c.Initialize(ctx, nil)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !strings.Contains(string(raw), `"test-device"`) {
		t.Errorf("result missing serverInfo: %s", raw)
	}
}

func TestListToolsPaginates(t *testing.T) {
	dev := newFakeDevice()
	pages := []ListToolsResult{
		{Tools: []ToolDescriptor{{Name: "a"}, {Name: "b"}}, NextCursor: "p2"},
		{Tools: []ToolDescriptor{{Name: "c"}}, NextCursor: ""},
	}
	calls := 0
	dev.on("tools/list", func(_ Request) Response {
		raw, _ := json.Marshal(pages[calls])
		calls++
		return Response{Result: raw}
	})
	c := NewClient(dev.send)
	dev.c = c

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	tools, err := c.ListTools(ctx, false)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if calls != 2 {
		t.Errorf("device.calls=%d, want 2", calls)
	}
	if len(tools) != 3 || tools[0].Name != "a" || tools[2].Name != "c" {
		t.Errorf("tools: %+v", tools)
	}
}

func TestCallToolReturnsText(t *testing.T) {
	dev := newFakeDevice()
	dev.on("tools/call", func(req Request) Response {
		var p struct{ Name string }
		_ = json.Unmarshal(req.Params, &p)
		raw, _ := json.Marshal(CallToolResult{
			Content: []ContentBlock{{Type: "text", Text: "volume set to 60 (tool=" + p.Name + ")"}},
		})
		return Response{Result: raw}
	})
	c := NewClient(dev.send)
	dev.c = c

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, "self.audio.set_volume", json.RawMessage(`{"volume":60}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if want := "volume set to 60 (tool=self.audio.set_volume)"; res.Text() != want {
		t.Errorf("Text()=%q, want %q", res.Text(), want)
	}
}

func TestCallToolPropagatesRPCError(t *testing.T) {
	dev := newFakeDevice()
	dev.on("tools/call", func(_ Request) Response {
		return Response{Error: &RPCError{Code: -1, Message: "tool not found"}}
	})
	c := NewClient(dev.send)
	dev.c = c

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := c.CallTool(ctx, "missing", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var rpc *RPCError
	if !errors.As(err, &rpc) {
		t.Errorf("expected *RPCError, got %T", err)
	}
}

func TestContextCancelReleasesCaller(t *testing.T) {
	// Sender that never gets a response.
	c := NewClient(func(_ context.Context, _ []byte) error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.CallTool(ctx, "any", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("call took too long: %v", time.Since(start))
	}
}
