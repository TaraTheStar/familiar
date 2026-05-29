// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mcp implements the JSON-RPC 2.0 subset of MCP we use to talk
// to the device's tool registry. The device acts as the MCP "server"
// (it exposes tools); we act as the MCP "client" (we list and call them).
//
// MCP messages ride inside the existing WS as type=mcp envelopes. This
// package doesn't know about WebSockets — it just produces and consumes
// JSON-RPC payloads. The session glues those to the wire.
package mcp

import "encoding/json"

// MCPVersion is what we declare in the initialize handshake. The device
// firmware references "2024-11-05" in mcp_server.cc.
const MCPVersion = "2024-11-05"

// Permission classifies tools so the LLM-facing layer can decide which
// to expose. Mirrors the user_only flag in the firmware's tool registry.
type Permission string

const (
	PermissionPublic   Permission = "public"
	PermissionUserOnly Permission = "user_only"
)

// ToolDescriptor is one entry from a tools/list response. We keep the
// input schema as RawMessage; the LLM client passes it through to the
// chat-completions endpoint, where the LLM consumes it directly.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ListToolsResult is the unmarshaled .result of a tools/list response.
type ListToolsResult struct {
	Tools      []ToolDescriptor `json:"tools"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// CallToolResult is the unmarshaled .result of a tools/call response.
// MCP tool results can be:
//
//   - A `content` array of typed blocks (text, image, resource)
//   - A simple `isError` flag
//
// For our voice-loop use case, callers mostly care about flattened text,
// which Text() returns.
type CallToolResult struct {
	Content []ContentBlock `json:"content,omitempty"`
	IsError bool           `json:"isError,omitempty"`
}

// ContentBlock is one item in CallToolResult.Content.
type ContentBlock struct {
	Type string `json:"type"`           // "text", "image", "resource"
	Text string `json:"text,omitempty"` // for type="text"
	// We don't model image/resource yet — the device's tools all return
	// text or simple JSON-stringified blobs. Add when needed.
}

// Text returns the concatenation of all text content blocks. Empty if
// the result was non-text (image, resource) or empty.
func (r CallToolResult) Text() string {
	var s string
	for _, b := range r.Content {
		if b.Type == "text" {
			s += b.Text
		}
	}
	return s
}

// -- JSON-RPC envelopes ------------------------------------------------------
//
// We expose these (and the marshal/unmarshal helpers below) so the session
// can splice the MCP payload into its own {type:"mcp", payload:...} frame.

type Request struct {
	JSONRPC string          `json:"jsonrpc"` // "2.0"
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return "mcp: " + e.Message }
