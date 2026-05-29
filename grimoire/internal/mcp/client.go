// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Sender writes one JSON-RPC payload as the body of a {type:"mcp",
// payload:...} WebSocket frame. The session implements this with its
// existing writeJSON helper.
type Sender func(ctx context.Context, payload []byte) error

// Client is one MCP session against a single device. Per-session, not
// shared. Routes incoming responses to the goroutine that issued the
// matching request via the pending-id map.
type Client struct {
	send Sender

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int]chan Response
}

// NewClient constructs a client that sends via the supplied function.
// The caller is responsible for plumbing incoming WS frames of
// type=mcp into HandleIncoming.
func NewClient(sender Sender) *Client {
	return &Client{
		send:    sender,
		pending: make(map[int]chan Response),
	}
}

// Initialize sends the MCP initialize handshake and waits for the
// device's serverInfo response. Caller can pass capabilities (e.g. a
// vision URL); pass nil for none.
func (c *Client) Initialize(ctx context.Context, capabilities json.RawMessage) (json.RawMessage, error) {
	params := struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities,omitempty"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}{
		ProtocolVersion: MCPVersion,
		Capabilities:    capabilities,
	}
	params.ClientInfo.Name = "stackend"
	params.ClientInfo.Version = "0.1.0"

	resp, err := c.call(ctx, "initialize", params)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ListTools pulls the full tool list, paginating via cursor until the
// device stops returning one. user_only tools are NOT included by
// default (matching the device's listing behavior); pass withUserTools
// to include them (admin operations like self.reboot).
func (c *Client) ListTools(ctx context.Context, withUserTools bool) ([]ToolDescriptor, error) {
	var all []ToolDescriptor
	cursor := ""
	for {
		params := struct {
			Cursor        string `json:"cursor,omitempty"`
			WithUserTools bool   `json:"withUserTools,omitempty"`
		}{Cursor: cursor, WithUserTools: withUserTools}

		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var result ListToolsResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("mcp: parse tools/list result: %w", err)
		}
		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			return all, nil
		}
		cursor = result.NextCursor
	}
}

// CallTool invokes one tool by name with the given arguments and
// returns the unmarshaled result.
//
// Arguments are passed as RawMessage so the caller (LLM tool-call
// dispatcher) can hand through the model's exact JSON without
// re-marshaling.
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (CallToolResult, error) {
	if arguments == nil {
		arguments = json.RawMessage(`{}`)
	}
	params := struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{Name: name, Arguments: arguments}

	raw, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return CallToolResult{}, err
	}
	var result CallToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return CallToolResult{}, fmt.Errorf("mcp: parse tools/call result: %w", err)
	}
	return result, nil
}

// HandleIncoming is called by the session when a {type:"mcp"} frame
// arrives. Routes the payload to the goroutine waiting on its ID.
// Unknown IDs (no matching call) are logged via the discarded channel
// behavior — silently dropped, which is correct for unsolicited
// notifications.
func (c *Client) HandleIncoming(payload []byte) {
	var resp Response
	if err := json.Unmarshal(payload, &resp); err != nil {
		// Not a response (could be a request from the device — MCP is
		// bidirectional but we don't expose tools from our side yet).
		return
	}
	if resp.JSONRPC != "2.0" {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[resp.ID]
	if ok {
		delete(c.pending, resp.ID)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	// Non-blocking send to a buffered channel (cap 1).
	select {
	case ch <- resp:
	default:
	}
}

// Close cancels all pending requests by closing their channels with a
// generic error response. Call when the WS dies so callers don't hang.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		select {
		case ch <- Response{ID: id, Error: &RPCError{Message: "client closed"}}:
		default:
		}
		close(ch)
		delete(c.pending, id)
	}
}

// call is the request/response inner loop. Allocates an ID, registers
// a pending channel, sends, waits for response or ctx done, returns
// the .Result bytes.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := int(c.nextID.Add(1))

	pBytes, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal params: %w", err)
	}
	req := Request{JSONRPC: "2.0", ID: id, Method: method, Params: pBytes}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal request: %w", err)
	}

	ch := make(chan Response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	// Defer cleanup so a cancelled context doesn't leak the pending entry.
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.send(ctx, reqBytes); err != nil {
		return nil, fmt.Errorf("mcp: send %s: %w", method, err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errors.New("mcp: client closed")
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
