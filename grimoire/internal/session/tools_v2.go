// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
)

// v2ToolPort implements toolPort over v2's first-class tool messages: tool_list
// and tool_call are top-level WS frames correlated by `id`, with no JSON-RPC
// envelope and no session_id (PROTOCOL_V2 §6). It mirrors mcp.Client's
// request/response correlation — a pending-id map routes each device response
// back to the goroutine that issued the request — but on the flat v2 wire.
type v2ToolPort struct {
	conn *websocket.Conn
	log  *slog.Logger

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int]chan protov2.Message
}

func newV2ToolPort(conn *websocket.Conn, log *slog.Logger) *v2ToolPort {
	return &v2ToolPort{
		conn:    conn,
		log:     log,
		pending: make(map[int]chan protov2.Message),
	}
}

// Discover paginates tool_list until the device stops returning a cursor
// (PROTOCOL_V2 §6.2).
func (p *v2ToolPort) Discover(ctx context.Context) ([]toolDescriptor, error) {
	var all []toolDescriptor
	var cursor *string
	for {
		id := int(p.nextID.Add(1))
		resp, err := p.request(ctx, id, protov2.ToolList{Type: "tool_list", ID: id, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		tl, ok := resp.(protov2.ToolList)
		if !ok {
			return nil, fmt.Errorf("tool_list %d: unexpected response %T", id, resp)
		}
		if tl.Error != nil {
			return nil, fmt.Errorf("tool_list: %s: %s", tl.Error.Code, tl.Error.Message)
		}
		if tl.Result == nil {
			return all, nil
		}
		for _, d := range tl.Result.Tools {
			all = append(all, toolDescriptorFromV2(d))
		}
		if tl.Result.NextCursor == "" {
			return all, nil
		}
		next := tl.Result.NextCursor
		cursor = &next
	}
}

// toolDescriptorFromV2 normalizes one v2 wire descriptor (tool_list result or
// the hello's tools_inline, §6.4) to the protocol-agnostic internal form.
func toolDescriptorFromV2(d protov2.ToolDescriptor) toolDescriptor {
	return toolDescriptor{
		Name:        d.Name,
		Description: d.Description,
		Schema:      d.ArgsSchema,
		Permission:  d.Permission,
	}
}

// CallTool dispatches a tool_call and flattens the result to text (§6.3). v2
// results are raw JSON (true, an object, a string); see toolResultText.
func (p *v2ToolPort) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if args == nil {
		args = json.RawMessage(`{}`)
	}
	id := int(p.nextID.Add(1))
	resp, err := p.request(ctx, id, protov2.ToolCall{Type: "tool_call", ID: id, Name: name, Args: args})
	if err != nil {
		return "", err
	}
	tc, ok := resp.(protov2.ToolCall)
	if !ok {
		return "", fmt.Errorf("tool_call %d: unexpected response %T", id, resp)
	}
	if tc.Error != nil {
		return "", fmt.Errorf("%s: %s", tc.Error.Code, tc.Error.Message)
	}
	return toolResultText(tc.Result), nil
}

// HandleIncoming decodes an inbound tool frame and routes it to the goroutine
// awaiting its id. Frames with no matching pending id (notifications, or device
// tool_call requests we don't serve yet — §6.5) are dropped, matching the MCP
// client's unknown-id behavior.
func (p *v2ToolPort) HandleIncoming(data []byte) {
	msg, err := protov2.Decode(data)
	if err != nil {
		return
	}
	var id int
	switch m := msg.(type) {
	case protov2.ToolList:
		id = m.ID
	case protov2.ToolCall:
		id = m.ID
	default:
		return
	}
	p.mu.Lock()
	ch, ok := p.pending[id]
	if ok {
		delete(p.pending, id)
	}
	p.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- msg:
	default:
	}
}

// Close releases all pending calls so request() unblocks with an error instead
// of hanging when the socket dies.
func (p *v2ToolPort) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, ch := range p.pending {
		close(ch)
		delete(p.pending, id)
	}
}

// request sends a tool request and waits for the response correlated by id, or
// the context's cancellation. The pending entry is registered before the send
// so a fast device reply can't race ahead of it.
func (p *v2ToolPort) request(ctx context.Context, id int, msg any) (protov2.Message, error) {
	ch := make(chan protov2.Message, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()

	if err := writeJSON(ctx, p.conn, msg); err != nil {
		return nil, fmt.Errorf("send tool request %d: %w", id, err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errors.New("tool port closed")
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// toolResultText flattens a v2 tool result (raw JSON) to the text the loop
// feeds back to the LLM. A bare string yields its value; null/true/empty yield
// "ok" (setter-style success); anything else is passed through as compact JSON.
func toolResultText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "true" {
		return "ok"
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	return s
}
