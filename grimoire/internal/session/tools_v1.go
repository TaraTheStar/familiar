// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/mcp"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
)

// v1ToolPort implements toolPort over MCP: the device is the MCP server, we are
// the client, and the JSON-RPC rides inside {type:"mcp", payload:...} frames.
// It is a thin adapter over mcp.Client — the proven correlation/pagination
// logic stays there; this just maps it onto the protocol-agnostic seam and
// owns the MCP-specific initialize handshake and vision capability plumbing.
type v1ToolPort struct {
	client      *mcp.Client
	log         *slog.Logger
	visionURL   string
	visionToken string
}

// newV1ToolPort wires an mcp.Client whose Sender wraps each JSON-RPC payload in
// the v1 {type:"mcp"} envelope (echoing the session_id, a v1 wart). The session
// routes inbound type=mcp frames back via HandleIncoming.
func newV1ToolPort(conn *websocket.Conn, sessionID, visionURL, visionToken string, log *slog.Logger) *v1ToolPort {
	client := mcp.NewClient(func(ctx context.Context, payload []byte) error {
		return writeJSON(ctx, conn, protocol.MCP{
			SessionID: sessionID,
			Type:      "mcp",
			Payload:   payload,
		})
	})
	return &v1ToolPort{client: client, log: log, visionURL: visionURL, visionToken: visionToken}
}

// Discover runs the MCP initialize handshake (advertising the vision callback
// if configured) then paginates tools/list. user_only tools are excluded,
// matching the device's default listing.
func (p *v1ToolPort) Discover(ctx context.Context) ([]toolDescriptor, error) {
	var capabilities json.RawMessage
	if p.visionURL != "" {
		vis := map[string]string{"url": p.visionURL}
		if p.visionToken != "" {
			vis["token"] = p.visionToken
		}
		capabilities, _ = json.Marshal(map[string]any{"vision": vis})
	}

	if _, err := p.client.Initialize(ctx, capabilities); err != nil {
		return nil, err
	}
	descs, err := p.client.ListTools(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]toolDescriptor, 0, len(descs))
	for _, d := range descs {
		out = append(out, toolDescriptor{
			Name:        d.Name,
			Description: d.Description,
			Schema:      d.InputSchema,
		})
	}
	return out, nil
}

// CallTool dispatches a tools/call and flattens the result to text. An empty
// (setter-style) result is reported as "ok" so the LLM sees success.
func (p *v1ToolPort) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	result, err := p.client.CallTool(ctx, name, args)
	if err != nil {
		return "", err
	}
	text := result.Text()
	if text == "" {
		text = "ok"
	}
	return text, nil
}

// HandleIncoming routes an inbound MCP payload to the awaiting call.
func (p *v1ToolPort) HandleIncoming(payload []byte) { p.client.HandleIncoming(payload) }

// Close cancels pending MCP calls.
func (p *v1ToolPort) Close() { p.client.Close() }
