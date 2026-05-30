// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
)

// This file defines the tool seam, the device-tool analogue of the deviceOut /
// wireDecoder seam in wire.go. The voice loop (turn.go) discovers and calls
// device tools only through toolPort and never touches MCP or the v2 tool wire
// directly. v1's port (tools_v1.go) wraps the MCP client; v2's port
// (tools_v2.go) speaks first-class tool_list / tool_call messages. At phase 4
// the v1 port and the dispatch branch are deleted and the loop is untouched.
//
// Server-side ("local") tools like get_current_time are NOT part of this seam —
// they are dispatched in the loop before it ever reaches the port (see
// dispatchToolCall). The port is exclusively the device's tool registry.

// toolDescriptor is one device tool, normalized across protocols. v1's MCP
// descriptor (inputSchema, no permission) and v2's descriptor (args_schema,
// permission) both reduce to this.
type toolDescriptor struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Permission  string // "" under v1 (MCP doesn't carry it in the descriptor)
}

// toolPort is everything the loop needs from the device's tool registry:
// discover the catalog once at session open, call tools during a turn, route
// inbound responses, and release pending calls at close.
type toolPort interface {
	// Discover returns the device's full tool catalog (paginating internally).
	Discover(ctx context.Context) ([]toolDescriptor, error)
	// CallTool invokes one device tool, returning its result as text (or an
	// error the loop surfaces back to the LLM).
	CallTool(ctx context.Context, name string, args json.RawMessage) (string, error)
	// HandleIncoming routes an inbound tool frame to the goroutine awaiting its
	// id. v1 receives the MCP payload (evMCP); v2 the whole frame (evToolResponse).
	HandleIncoming(data []byte)
	// Close releases all pending calls so the loop never hangs on a dead socket.
	Close()
}

// initTools discovers the device tool catalog through the active port and
// converts it to the llm.Tool form the LLM consumes, plus a name index. Best-
// effort: discovery failure only means the session runs without device tools
// (server-side local tools still work). MUST run on a goroutine — discovery
// waits on responses that arrive on the read loop.
func (s *Session) initTools(ctx context.Context) {
	if s.toolPort == nil {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, s.cfg.MCPInitTimeout)
	defer cancel()

	descs, err := s.toolPort.Discover(dctx)
	if err != nil {
		s.log.Warn("tool discovery failed; running without device tools",
			"err", err, "protocol", s.protocolVersion)
		return
	}

	tools := make([]llm.Tool, 0, len(descs))
	byName := make(map[string]toolDescriptor, len(descs))
	skipped := 0
	for _, d := range descs {
		// Only public tools are exposed to the LLM (PROTOCOL_V2 §6.1): user_only
		// is for explicit admin operations and system_only is server-internal.
		// v1 MCP descriptors carry no permission (the device already filtered
		// user_only in ListTools), so "" counts as public.
		if !exposedToLLM(d.Permission) {
			skipped++
			continue
		}
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Schema,
			},
		})
		byName[d.Name] = d
	}
	s.toolsMu.Lock()
	s.tools = tools
	s.toolByName = byName
	s.toolsMu.Unlock()

	s.log.Info("tools ready", "count", len(tools), "skipped_non_public", skipped, "protocol", s.protocolVersion)
}

// exposedToLLM reports whether a tool of the given permission may be advertised
// to the LLM. Only public (or unset, treated as public) tools qualify.
func exposedToLLM(permission string) bool {
	return permission == "" || permission == "public"
}
