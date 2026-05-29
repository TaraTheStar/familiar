// SPDX-License-Identifier: AGPL-3.0-or-later

// Package protocol defines the v1 wire format spoken between the StackChan
// firmware and this server. The full normative reference lives at
// ../../../StackChan/SERVER_CONTRACT.md.
//
// JSON messages flow in both directions on a WebSocket; raw Opus audio rides
// the same socket as binary frames. This file defines the JSON shapes only.
package protocol

import "encoding/json"

// Direction conventions:
//
//   Incoming = device → server (this package decodes these)
//   Outgoing = server → device (this package encodes these)
//
// Every message has a `type` field; device→server messages additionally echo
// the `session_id` from the server hello (v1 wart we live with).

// ----------------------------------------------------------------------------
// Shared building blocks
// ----------------------------------------------------------------------------

// AudioParams describes one direction of the audio stream. Used in both
// client and server hello messages.
type AudioParams struct {
	Format        string `json:"format,omitempty"`         // "opus"
	SampleRate    int    `json:"sample_rate,omitempty"`    // Hz
	Channels      int    `json:"channels,omitempty"`       // 1
	FrameDuration int    `json:"frame_duration,omitempty"` // ms
}

// Features lists optional capabilities the client advertises in its hello.
type Features struct {
	AEC bool `json:"aec,omitempty"`
	MCP bool `json:"mcp,omitempty"`
}

// ----------------------------------------------------------------------------
// Incoming: device → server
// ----------------------------------------------------------------------------

// ClientHello is the first message the device sends after WS connect.
type ClientHello struct {
	Type        string       `json:"type"`      // "hello"
	Version     int          `json:"version"`   // protocol version (currently 1)
	Transport   string       `json:"transport"` // "websocket"
	Features    *Features    `json:"features,omitempty"`
	AudioParams *AudioParams `json:"audio_params,omitempty"`
}

// Listen carries the three sub-types of listening-state events from the device:
// state="start" (begin streaming mic audio), "stop" (mic audio ended),
// or "detect" (wake word fired, optional `text` is the phrase).
type Listen struct {
	SessionID string `json:"session_id"`
	Type      string `json:"type"`           // "listen"
	State     string `json:"state"`          // "start" | "stop" | "detect"
	Mode      string `json:"mode,omitempty"` // "auto" | "manual" | "realtime"
	Text      string `json:"text,omitempty"` // wake-word phrase when state=="detect"
}

// Abort signals user barge-in: device wants the server to stop streaming TTS
// and prepare for a fresh turn.
type Abort struct {
	SessionID string `json:"session_id"`
	Type      string `json:"type"`             // "abort"
	Reason    string `json:"reason,omitempty"` // "wake_word_detected" | ""
}

// MCP is the envelope used both directions; the JSON-RPC 2.0 payload is
// opaque at this layer and parsed by package mcp.
type MCP struct {
	SessionID string          `json:"session_id,omitempty"`
	Type      string          `json:"type"` // "mcp"
	Payload   json.RawMessage `json:"payload"`
}

// Event is a dotty-branch addition for ambient telemetry (face_seen, etc).
// `Data` is event-specific JSON kept opaque at the protocol layer.
type Event struct {
	SessionID string          `json:"session_id"`
	Type      string          `json:"type"` // "event"
	Name      string          `json:"name"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// ----------------------------------------------------------------------------
// Outgoing: server → device
// ----------------------------------------------------------------------------

// ServerHello is the server's reply to ClientHello. Negotiates session ID
// and the audio parameters the server will use for TTS output.
type ServerHello struct {
	Type        string       `json:"type"`      // "hello"
	Transport   string       `json:"transport"` // "websocket"
	SessionID   string       `json:"session_id"`
	AudioParams *AudioParams `json:"audio_params,omitempty"`
}

// TTS drives the device's speaking state machine.
//
//	state="start"          → reset abort flag, audio frames coming
//	state="sentence_start" → text is a caption chunk; device flips to Speaking
//	state="stop"           → audio done; device returns to Listening (or Idle)
type TTS struct {
	Type  string `json:"type"`           // "tts"
	State string `json:"state"`          // "start" | "sentence_start" | "stop"
	Text  string `json:"text,omitempty"` // only for state=="sentence_start"
}

// STT is the ASR result frame. Display-only on device.
type STT struct {
	Type string `json:"type"` // "stt"
	Text string `json:"text"`
}

// LLM carries the emotion tag that drives the avatar expression.
type LLM struct {
	Type    string `json:"type"` // "llm"
	Emotion string `json:"emotion"`
}

// System carries device-level commands. Only "reboot" is currently honored.
type System struct {
	Type    string `json:"type"`    // "system"
	Command string `json:"command"` // "reboot"
}

// Alert renders a full-screen popup with sound. All three text fields required
// by the device parser.
type Alert struct {
	Type    string `json:"type"` // "alert"
	Status  string `json:"status"`
	Message string `json:"message"`
	Emotion string `json:"emotion"`
}

// ----------------------------------------------------------------------------
// OTA HTTP types (not WebSocket — see internal/ota for the handler)
// ----------------------------------------------------------------------------

// OTAResponse is what the device's HTTP discovery call receives. Only
// WebSocket and Firmware are practically required for a self-hosted setup;
// ServerTime is recommended (saves the device an NTP round-trip).
type OTAResponse struct {
	WebSocket  *WebSocketConfig `json:"websocket,omitempty"`
	ServerTime *ServerTime      `json:"server_time,omitempty"`
	Firmware   *Firmware        `json:"firmware,omitempty"`
}

// WebSocketConfig tells the device where to open the live session.
type WebSocketConfig struct {
	URL     string `json:"url"`               // "ws://host:port/path"
	Token   string `json:"token,omitempty"`   // optional Bearer token
	Version int    `json:"version,omitempty"` // binary framing version (1, 2, 3)
}

// ServerTime sets the device clock. Unix milliseconds + signed minutes from UTC.
type ServerTime struct {
	Timestamp      int64 `json:"timestamp"`
	TimezoneOffset int   `json:"timezone_offset"`
}

// Firmware advertises the canonical version + URL. Echo the device's current
// version to suppress the OTA path; bump version to trigger upgrade.
type Firmware struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	Force   int    `json:"force,omitempty"`
}
