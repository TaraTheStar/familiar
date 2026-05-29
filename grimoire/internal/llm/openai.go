// SPDX-License-Identifier: AGPL-3.0-or-later

// Package llm implements a streaming client for OpenAI-compatible
// /v1/chat/completions endpoints (which is what llama-swap exposes).
//
// Supports both plain text streaming AND native OpenAI tool calling
// (functions). The Stream() iterator yields StreamEvents that are
// either content deltas or completed tool calls.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"
)

// base64Encode is a thin wrapper so dataURIJPEG stays readable.
func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// Role is the OpenAI chat-completions role tag.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry in the chat-completions messages array. Same
// struct serves all roles. JSON shape depends on which fields are set:
//
//	system/user/assistant text:  {role, content:"..."}
//	user multimodal:             {role:"user", content:[{type:"text"...},{type:"image_url"...}]}
//	assistant tool call:         {role:"assistant", tool_calls:[...]}
//	tool result:                 {role:"tool", tool_call_id, name?, content:"..."}
//
// ContentParts takes precedence over Content when both are set: callers
// use Content for plain text and ContentParts for multimodal (image)
// messages. See UserMultimodal() for the common case.
type Message struct {
	Role         Role          `json:"-"`
	Content      string        `json:"-"`
	ContentParts []ContentPart `json:"-"`
	ToolCalls    []ToolCall    `json:"-"`
	ToolCallID   string        `json:"-"`
	Name         string        `json:"-"`
}

// ContentPart is one element of an OpenAI-style array content. We model
// only the two parts we use: text and image_url.
type ContentPart struct {
	Type     string    `json:"type"`                // "text" | "image_url"
	Text     string    `json:"text,omitempty"`      // for type="text"
	ImageURL *ImageURL `json:"image_url,omitempty"` // for type="image_url"
}

// ImageURL holds either a data: URI (base64-encoded inline image) or
// an http(s) URL the model can fetch. We always use data: URIs because
// the LLM container can't reach the device.
type ImageURL struct {
	URL string `json:"url"`
}

// UserMultimodal builds a "user" message with text + one inline image.
// The image bytes are base64-encoded into a data: URI.
func UserMultimodal(text string, jpeg []byte) Message {
	return Message{
		Role: RoleUser,
		ContentParts: []ContentPart{
			{Type: "text", Text: text},
			{Type: "image_url", ImageURL: &ImageURL{URL: dataURIJPEG(jpeg)}},
		},
	}
}

// MarshalJSON renders the dual-shape content field. If ContentParts is
// non-empty it goes out as an array; otherwise Content goes out as a
// plain string (omitted when empty and tool_calls is set).
func (m Message) MarshalJSON() ([]byte, error) {
	type wire struct {
		Role       Role       `json:"role"`
		Content    any        `json:"content,omitempty"`
		ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
		Name       string     `json:"name,omitempty"`
	}
	w := wire{
		Role:       m.Role,
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
	switch {
	case len(m.ContentParts) > 0:
		w.Content = m.ContentParts
	case m.Content != "":
		w.Content = m.Content
	case len(m.ToolCalls) == 0 && m.ToolCallID == "":
		// Empty content + no tool fields — OpenAI requires content for
		// system/user. Emit empty string explicitly.
		w.Content = ""
	}
	return json.Marshal(w)
}

// dataURIJPEG returns "data:image/jpeg;base64,<encoded>".
func dataURIJPEG(jpeg []byte) string {
	return "data:image/jpeg;base64," + base64Encode(jpeg)
}

// Tool describes one function the LLM may call. Mirrors OpenAI's
// "function" tool type — the only one we use.
type Tool struct {
	Type     string       `json:"type"` // always "function" today
	Function ToolFunction `json:"function"`
}

// ToolFunction is the schema the LLM uses to decide whether to call.
// Parameters is a JSON Schema object (RawMessage so the caller can
// pass through whatever the MCP server provided unchanged).
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is the on-wire shape of an LLM-emitted function call. We
// keep Arguments as a string per OpenAI spec (it's JSON, but the LLM
// emits it as a string so we don't double-parse).
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the inner block of a ToolCall.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded as a string
}

// StreamEvent is one item yielded by Client.Stream. Exactly one of
// Content or ToolCall is set per event.
//
//   - Content delta: ToolCall == nil, Content != ""
//   - Completed tool call: ToolCall != nil, Content == ""
//
// Stream finishes by closing the iterator (no sentinel event).
type StreamEvent struct {
	Content  string
	ToolCall *ToolCall
}

// Client speaks the OpenAI chat-completions wire format. Suitable for
// any compatible backend; we point it at llama-swap.
type Client struct {
	// BaseURL is the root, e.g. http://192.0.2.20:8080. /v1/chat/completions
	// is appended.
	BaseURL string

	// Model is the model name registered with the backend (e.g.
	// "gemma4-26B-A4B" for the current llama-swap config).
	Model string

	// APIKey is sent as "Authorization: Bearer <key>". llama-swap accepts
	// any non-empty value but the header has to be present.
	APIKey string

	// MaxTokens caps the response length; 0 = backend default.
	MaxTokens int

	// Temperature in [0,2]; 0 = omit (backend default).
	Temperature float64

	// HTTP is the underlying transport; nil → a default with a generous
	// timeout.
	HTTP *http.Client
}

// Stream sends a chat-completions request and yields content deltas
// plus completed tool calls as a single iterator. Range over it:
//
//	for ev, err := range client.Stream(ctx, msgs, tools) {
//	    if err != nil { ... }
//	    if ev.ToolCall != nil { ... dispatch ... }
//	    if ev.Content != "" { ... append ... }
//	}
//
// On error, yields one final (zero StreamEvent, err) and stops.
func (c *Client) Stream(ctx context.Context, msgs []Message, tools []Tool) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		body := chatRequest{
			Model:    c.Model,
			Messages: msgs,
			Stream:   true,
			Tools:    tools,
		}
		if c.MaxTokens > 0 {
			body.MaxTokens = c.MaxTokens
		}
		if c.Temperature > 0 {
			body.Temperature = c.Temperature
		}

		buf, err := json.Marshal(body)
		if err != nil {
			yield(StreamEvent{}, fmt.Errorf("llm: marshal: %w", err))
			return
		}

		req, err := http.NewRequestWithContext(ctx, "POST",
			c.BaseURL+"/v1/chat/completions", bytes.NewReader(buf))
		if err != nil {
			yield(StreamEvent{}, fmt.Errorf("llm: new request: %w", err))
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}

		client := c.HTTP
		if client == nil {
			client = &http.Client{Timeout: 5 * time.Minute}
		}

		resp, err := client.Do(req)
		if err != nil {
			yield(StreamEvent{}, fmt.Errorf("llm: request: %w", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			yield(StreamEvent{}, fmt.Errorf("llm: status %d: %s", resp.StatusCode, errBody))
			return
		}

		// Accumulator for tool calls — keyed by their `index` since the
		// fields arrive across multiple SSE chunks.
		tcAccum := map[int]*ToolCall{}
		// Track insertion order so we emit completed calls in the order
		// the LLM produced them.
		var tcOrder []int

		flushToolCalls := func() bool {
			for _, idx := range tcOrder {
				tc := tcAccum[idx]
				if tc == nil || tc.Function.Name == "" {
					continue
				}
				// Make a stable copy for the yielded event.
				out := *tc
				if !yield(StreamEvent{ToolCall: &out}, nil) {
					return false
				}
			}
			// Reset for the next round (some servers stream tool calls,
			// then content, then more tool calls in the same response).
			tcAccum = map[int]*ToolCall{}
			tcOrder = tcOrder[:0]
			return true
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			payload, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			if payload == "[DONE]" {
				flushToolCalls()
				return
			}
			var ev chatChunk
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				yield(StreamEvent{}, fmt.Errorf("llm: parse SSE event: %w (payload: %s)", err, payload))
				return
			}
			if len(ev.Choices) == 0 {
				continue
			}
			ch := ev.Choices[0]

			// Content delta.
			if ch.Delta.Content != "" {
				if !yield(StreamEvent{Content: ch.Delta.Content}, nil) {
					return
				}
			}

			// Tool-call deltas: accumulate by index.
			for _, td := range ch.Delta.ToolCalls {
				idx := 0
				if td.Index != nil {
					idx = *td.Index
				}
				cur, exists := tcAccum[idx]
				if !exists {
					cur = &ToolCall{Type: "function"}
					tcAccum[idx] = cur
					tcOrder = append(tcOrder, idx)
				}
				if td.ID != "" {
					cur.ID = td.ID
				}
				if td.Type != "" {
					cur.Type = td.Type
				}
				if td.Function.Name != "" {
					cur.Function.Name = td.Function.Name
				}
				cur.Function.Arguments += td.Function.Arguments
			}

			// On finish_reason="tool_calls", emit the accumulated calls.
			// Some servers also use "stop" with content+tool_calls in
			// the same response; we flush on either terminal reason.
			if ch.FinishReason != "" {
				if !flushToolCalls() {
					return
				}
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
			yield(StreamEvent{}, fmt.Errorf("llm: scan stream: %w", err))
		}
	}
}

// Wire types (unexported; callers use Message + Stream).

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
}

type chatChunk struct {
	Choices []chatChoice `json:"choices"`
}

type chatChoice struct {
	Delta        chatDelta `json:"delta"`
	FinishReason string    `json:"finish_reason,omitempty"`
}

type chatDelta struct {
	Content   string          `json:"content,omitempty"`
	ToolCalls []chatToolDelta `json:"tool_calls,omitempty"`
}

type chatToolDelta struct {
	Index    *int              `json:"index,omitempty"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function chatToolDeltaFunc `json:"function,omitempty"`
}

type chatToolDeltaFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
