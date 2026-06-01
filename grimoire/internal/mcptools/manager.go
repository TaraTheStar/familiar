// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mcptools bridges external standard-MCP servers into grimoire's
// LLM tool catalog (PROTOCOL_V2 §6.5, §11-q5). A single process-global Manager
// connects to the configured servers at startup, lists their tools, and exposes
// them to the LLM as ordinary function tools — namespaced "mcp__<server>__<tool>"
// so they never collide with the device's own catalog. Tool calls are dispatched
// in-process to the owning MCP session; nothing is forwarded to the device. This
// is the LLM-facing direction only — grimoire still does not answer
// device-initiated tool_list/tool_call (§6.5 device-initiated remains a non-goal).
package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
)

// connectTimeout bounds the initialize + tools/list handshake per server. A
// server that doesn't answer in time is logged and skipped — it must never
// stall server startup or fail a device session.
const connectTimeout = 30 * time.Second

// ServerSpec configures one external MCP server.
type ServerSpec struct {
	// Name namespaces the server's tools ("mcp__<name>__<tool>"). Required.
	Name string `json:"name"`
	// Transport is "stdio" (default) or "http".
	Transport string `json:"transport,omitempty"`
	// Command + Args launch the server subprocess (stdio transport).
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Env are extra "KEY=VALUE" entries appended to the subprocess environment
	// (stdio transport) — e.g. API keys the MCP server needs.
	Env []string `json:"env,omitempty"`
	// URL is the endpoint for the streamable-HTTP transport.
	URL string `json:"url,omitempty"`
}

// Config is the on-disk MCP adapter configuration (the -mcp-config JSON).
type Config struct {
	Servers []ServerSpec `json:"servers"`
}

// serverTool maps a namespaced LLM tool name back to the MCP session and the
// real (un-namespaced) tool name to invoke on it.
type serverTool struct {
	session *mcp.ClientSession
	server  string
	rawName string
}

// Manager is the process-global external-MCP tool bridge. Built once at startup
// and shared across all device sessions. After New returns, tools/handlers are
// read-only, so Tools/Handles/Call need no lock; the MCP client sessions are
// themselves safe for concurrent calls.
type Manager struct {
	log      *slog.Logger
	tools    []llm.Tool
	handlers map[string]serverTool
	sessions []*mcp.ClientSession
}

// New connects to each configured server and aggregates their tools. A server
// that fails to connect or list tools is logged and skipped, never fatal — the
// server runs with whatever subset came up. Always returns a usable Manager.
func New(ctx context.Context, cfg Config, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{log: log, handlers: make(map[string]serverTool)}
	for _, spec := range cfg.Servers {
		transport, err := buildTransport(spec)
		if err != nil {
			log.Warn("mcp: bad server spec; skipping", "server", spec.Name, "err", err)
			continue
		}
		if err := m.connect(ctx, spec.Name, transport); err != nil {
			log.Warn("mcp: server unavailable; skipping", "server", spec.Name, "err", err)
		}
	}
	log.Info("mcp: adapter ready", "servers", len(m.sessions), "tools", len(m.tools))
	return m
}

// buildTransport turns a spec into the SDK transport for its declared kind.
func buildTransport(spec ServerSpec) (mcp.Transport, error) {
	if spec.Name == "" {
		return nil, errors.New("server name is required")
	}
	switch spec.Transport {
	case "stdio", "":
		if spec.Command == "" {
			return nil, errors.New("stdio transport requires a command")
		}
		cmd := exec.Command(spec.Command, spec.Args...)
		if len(spec.Env) > 0 {
			cmd.Env = append(os.Environ(), spec.Env...)
		}
		return &mcp.CommandTransport{Command: cmd}, nil
	case "http":
		if spec.URL == "" {
			return nil, errors.New("http transport requires a url")
		}
		return &mcp.StreamableClientTransport{Endpoint: spec.URL}, nil
	default:
		return nil, fmt.Errorf("unknown transport %q (want stdio|http)", spec.Transport)
	}
}

// connect establishes one MCP session over the given transport and registers
// its tools. The ctx bounds only the connect + discovery handshake; the session
// itself lives until Close (per the SDK contract). Used by New and by tests
// (which inject an in-memory transport).
func (m *Manager) connect(ctx context.Context, name string, transport mcp.Transport) error {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "stackchan-server", Version: "0.1.0"}, nil)
	sess, err := client.Connect(cctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	var tools []*mcp.Tool
	for cursor := ""; ; {
		res, err := sess.ListTools(cctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			_ = sess.Close()
			return fmt.Errorf("list tools: %w", err)
		}
		tools = append(tools, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}

	m.sessions = append(m.sessions, sess)
	added := 0
	for _, t := range tools {
		if t == nil || t.Name == "" {
			continue
		}
		namespaced := toolName(name, t.Name)
		var params json.RawMessage
		if t.InputSchema != nil {
			if b, err := json.Marshal(t.InputSchema); err == nil {
				params = b
			}
		}
		m.tools = append(m.tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        namespaced,
				Description: t.Description,
				Parameters:  params,
			},
		})
		m.handlers[namespaced] = serverTool{session: sess, server: name, rawName: t.Name}
		added++
	}
	m.log.Info("mcp: server connected", "server", name, "tools", added)
	return nil
}

// toolName builds the namespaced LLM tool name for an MCP tool.
func toolName(server, tool string) string { return "mcp__" + server + "__" + tool }

// Tools returns the aggregated LLM tool catalog (namespaced). Safe to call
// concurrently; the slice is built once at New and never mutated.
func (m *Manager) Tools() []llm.Tool { return m.tools }

// Handles reports whether name is one of this Manager's namespaced tools.
func (m *Manager) Handles(name string) bool {
	_, ok := m.handlers[name]
	return ok
}

// Call invokes a namespaced tool on its owning MCP session and flattens the
// result to text for the LLM. On a tool-reported error it returns the error
// text together with a non-nil error (the caller feeds the text to the LLM so
// it can adapt). args must be a JSON object; empty is treated as {}.
func (m *Manager) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	st, ok := m.handlers[name]
	if !ok {
		return "", fmt.Errorf("mcp: unknown tool %q", name)
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	res, err := st.session.CallTool(ctx, &mcp.CallToolParams{Name: st.rawName, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("mcp call %s: %w", name, err)
	}
	text := flattenContent(res.Content)
	if res.IsError {
		if text == "" {
			text = "the tool reported an error"
		}
		return text, fmt.Errorf("mcp tool %s error: %s", name, text)
	}
	if text == "" {
		text = "ok"
	}
	return text, nil
}

// flattenContent concatenates the text blocks of an MCP tool result (newline-
// separated), ignoring non-text content the LLM can't consume as a string.
func flattenContent(content []mcp.Content) string {
	var sb strings.Builder
	for _, c := range content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(tc.Text)
		}
	}
	return strings.TrimSpace(sb.String())
}

// Close shuts down every MCP session. Best-effort; errors are ignored since
// the process is exiting.
func (m *Manager) Close() error {
	for _, s := range m.sessions {
		_ = s.Close()
	}
	return nil
}
