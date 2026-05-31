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

	// Fast path (PROTOCOL_V2 §6.4 tools_inline): the device announced its
	// catalog in the hello, so register it and skip the tool_list round-trip.
	// Belt-and-suspenders: only take this path if ≥1 descriptor is structurally
	// usable (has a name). An inline list that is empty-after-filtering or all
	// nameless garbage falls through to tool_list discovery rather than leaving
	// the session toolless. (All-non-public-but-named inline is a deliberate
	// "expose nothing" and is honored — it does NOT fall back.)
	if len(s.inlineTools) > 0 {
		named := 0
		for _, d := range s.inlineTools {
			if d.Name != "" {
				named++
			}
		}
		if named > 0 {
			exposed := s.registerTools(s.inlineTools)
			s.log.Info("tools ready", "count", exposed, "announced", len(s.inlineTools),
				"source", "tools_inline")
			return
		}
		s.log.Warn("tools_inline announced but no usable descriptors; falling back to tool_list",
			"announced", len(s.inlineTools))
	}

	dctx, cancel := context.WithTimeout(ctx, s.cfg.MCPInitTimeout)
	defer cancel()

	descs, err := s.toolPort.Discover(dctx)
	if err != nil {
		s.log.Warn("tool discovery failed; running without device tools", "err", err)
		return
	}
	exposed := s.registerTools(descs)
	s.log.Info("tools ready", "count", exposed, "discovered", len(descs),
		"source", "tool_list")
}

// registerTools converts descriptors to the llm.Tool form the LLM consumes,
// plus a name index, and installs both as the session's catalog. Returns the
// number exposed to the LLM. A descriptor is skipped if it has no name
// (malformed) or its permission isn't LLM-exposable. Shared by the tools_inline
// fast path and tool_list discovery.
func (s *Session) registerTools(descs []toolDescriptor) (exposed int) {
	tools := make([]llm.Tool, 0, len(descs))
	byName := make(map[string]toolDescriptor, len(descs))
	for _, d := range descs {
		// Only public tools are exposed to the LLM (PROTOCOL_V2 §6.1): user_only
		// is for explicit admin operations and system_only is server-internal.
		// v1 MCP descriptors carry no permission (the device already filtered
		// user_only in ListTools), so "" counts as public. A nameless descriptor
		// is unusable (can't be called) and is dropped.
		if d.Name == "" || !exposedToLLM(d.Permission) {
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
	return len(tools)
}

// exposedToLLM reports whether a tool of the given permission may be advertised
// to the LLM. Only public (or unset, treated as public) tools qualify.
func exposedToLLM(permission string) bool {
	return permission == "" || permission == "public"
}
