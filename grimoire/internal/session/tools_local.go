// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"time"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
)

// Server-side tools.
//
// Most tools come from the device over MCP (self.camera.take_photo, volume,
// etc). A few capabilities the device can't provide — like the wall clock —
// are implemented here on the server and advertised to the LLM alongside the
// device tools. They're dispatched locally instead of being forwarded over
// MCP. See dispatchToolCall in turn.go for the routing.

// localToolHandler runs a server-side tool. It receives the raw JSON argument
// string the LLM emitted and returns the textual result fed back into the
// dialogue as the tool message.
type localToolHandler func(ctx context.Context, args json.RawMessage) (string, error)

// ToolProvider is an optional process-global source of extra LLM-callable tools
// beyond the device's own catalog and the per-session local tools above — e.g.
// external MCP servers bridged in (internal/mcptools). It is shared across all
// sessions; its tools are exposed to the LLM via snapshotTools and dispatched
// in-process via dispatchToolCall, never forwarded to the device. nil disables
// it. Tool names are expected to be namespaced so they can't collide with the
// device catalog. Implementations must be safe for concurrent use.
type ToolProvider interface {
	// Tools is the LLM tool catalog this provider contributes.
	Tools() []llm.Tool
	// Handles reports whether a tool name belongs to this provider.
	Handles(name string) bool
	// Call invokes the named tool. On a tool-level failure it returns descriptive
	// text alongside a non-nil error so the loop can feed the text to the LLM.
	Call(ctx context.Context, name string, args json.RawMessage) (string, error)
}

// buildLocalTools returns the server-side tool descriptors to advertise plus
// the handler map to dispatch them. Called once per session at construction.
func (s *Session) buildLocalTools() ([]llm.Tool, map[string]localToolHandler) {
	tools := []llm.Tool{
		{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "get_current_time",
				Description: "Get the current local date and time. Call this whenever the user asks what time it is, what the date is, or what day it is.",
				// No arguments.
				Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			},
		},
	}
	handlers := map[string]localToolHandler{
		"get_current_time": s.getCurrentTime,
	}
	return tools, handlers
}

// getCurrentTime reports the wall clock in the session's configured timezone.
// The result is phrased for text-to-speech (no ISO formatting).
func (s *Session) getCurrentTime(_ context.Context, _ json.RawMessage) (string, error) {
	loc := s.cfg.TimeLocation
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	return now.Format("Monday, January 2, 2006, 3:04 PM MST"), nil
}
