// SPDX-License-Identifier: AGPL-3.0-or-later

// Package protov2 defines the v2 wire format spoken between the StackChan
// firmware and this server. The normative reference is docs/PROTOCOL_V2.md.
//
// v2 keeps v1's transport choices — typed JSON in WebSocket text frames, raw
// Opus in binary frames — but fixes v1's warts: no per-message session_id (the
// connection is the session), the overloaded `listen` type is split into
// `listen_start`/`listen_stop`/`wake`, the device's speaking state is decoupled
// from captions via `audio_begin`/`audio_end`, tools are first-class messages
// (no JSON-RPC envelope), audio has credit-based flow control, and there is a
// first-class `error` type. This file defines the JSON shapes only; framing and
// flow control live in package session.
package protov2

import "encoding/json"

// Direction conventions (same as v1):
//
//	Incoming = device → server (Decode handles these)
//	Outgoing = server → device (marshaled and written by the session)
//
// Several types are bidirectional (goodbye, error, tool_list, tool_call): the
// same struct is both decoded inbound and marshaled outbound. Every message has
// a snake_case `type`. Requests that expect a typed reply carry a monotonic
// `id` (scoped per direction); the reply echoes `type`+`id` plus `result` or
// `error`. Notifications carry `type` only.

// ----------------------------------------------------------------------------
// Shared building blocks
// ----------------------------------------------------------------------------

// AudioStream describes one direction of the Opus stream. In the client hello
// all fields are populated; the server hello confirms with rate+frame_ms and
// leaves codec/channels implicit (omitempty).
type AudioStream struct {
	Codec    string `json:"codec,omitempty"`    // "opus"
	Rate     int    `json:"rate,omitempty"`     // Hz
	Channels int    `json:"channels,omitempty"` // 1
	FrameMS  int    `json:"frame_ms,omitempty"` // ms
}

// AudioConfig pairs the two stream directions. `in` is device→server
// (microphone); `out` is server→device (TTS).
type AudioConfig struct {
	In  AudioStream `json:"in"`
	Out AudioStream `json:"out"`
}

// ErrorBody is the nested error payload on a failed request/response
// (hello rejection, tool_call failure): {"error":{"code","message"}}. The
// standalone Error message (§4.9) is a different, top-level shape.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ----------------------------------------------------------------------------
// Handshake
// ----------------------------------------------------------------------------

// ClientInfo identifies the device in its hello. tools_inline (§6.4) lets the
// device announce its tool catalog up front, saving the server a tool_list
// round-trip.
type ClientInfo struct {
	Name        string           `json:"name,omitempty"`
	Version     string           `json:"version,omitempty"`
	DeviceID    string           `json:"device_id,omitempty"`
	UUID        string           `json:"uuid,omitempty"`
	ToolsInline []ToolDescriptor `json:"tools_inline,omitempty"`
}

// ClientHello is the device's first frame (§3.1).
type ClientHello struct {
	Type            string      `json:"type"` // "hello"
	ID              int         `json:"id"`
	Client          ClientInfo  `json:"client"`
	Audio           AudioConfig `json:"audio"`
	Features        []string    `json:"features,omitempty"`
	TelemetryEvents []string    `json:"telemetry_events,omitempty"`
}

// ServerInfo identifies the server in its hello reply.
type ServerInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// TimeSync sets the device clock from the server hello, removing v1's separate
// OTA server_time round-trip. Unix milliseconds + signed minutes from UTC.
type TimeSync struct {
	UnixMS      int64 `json:"unix_ms"`
	TZOffsetMin int   `json:"tz_offset_min"`
}

// FlowControl announces the initial server→device audio credit (§5.1): the
// number of binary frames the server may send before any refill.
type FlowControl struct {
	AudioCreditInitial int `json:"audio_credit_initial"`
}

// ServerHello is the server's reply to ClientHello (§3.2). On success Result is
// "ok"; on negotiation failure Error is set and the device closes.
type ServerHello struct {
	Type        string       `json:"type"` // "hello"
	ID          int          `json:"id"`
	Result      string       `json:"result,omitempty"` // "ok"
	Error       *ErrorBody   `json:"error,omitempty"`
	Server      *ServerInfo  `json:"server,omitempty"`
	Audio       *AudioConfig `json:"audio,omitempty"`
	Features    []string     `json:"features,omitempty"`
	Time        *TimeSync    `json:"time,omitempty"`
	VisionURL   string       `json:"vision_url,omitempty"`
	FlowControl *FlowControl `json:"flow_control,omitempty"`
}

// ----------------------------------------------------------------------------
// Incoming: device → server
// ----------------------------------------------------------------------------

// ListenStart begins a capture window (§4.2). Mode: "auto" | "manual" |
// "realtime".
type ListenStart struct {
	Type string `json:"type"` // "listen_start"
	Mode string `json:"mode,omitempty"`
}

// ListenStop ends a capture window (§4.2).
type ListenStop struct {
	Type string `json:"type"` // "listen_stop"
}

// Wake is a wake-word detection (§4.2). From idle it opens the turn's audio
// window when the device advertised the wake_word_audio feature; during TTS it
// is a barge-in, followed by Abort.
type Wake struct {
	Type   string  `json:"type"` // "wake"
	Phrase string  `json:"phrase,omitempty"`
	Score  float64 `json:"score,omitempty"`
}

// Abort signals user barge-in (§4.2): stop streaming TTS and prepare a fresh
// turn. Reason: "wake" | "user_action" | "timeout" | null.
type Abort struct {
	Type   string `json:"type"` // "abort"
	Reason string `json:"reason,omitempty"`
}

// Telemetry is an ambient perception notification (§4.8). Legal Event names are
// declared in the client hello's telemetry_events; Data is event-specific.
type Telemetry struct {
	Type  string          `json:"type"` // "telemetry"
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// AudioCredit refills the server's send budget for server→device audio (§5.2).
type AudioCredit struct {
	Type   string `json:"type"` // "audio_credit"
	Frames int    `json:"frames"`
}

// ----------------------------------------------------------------------------
// Outgoing: server → device
// ----------------------------------------------------------------------------

// Transcript is an ASR result (§4.3). Display-only; drives the thinking
// emotion. Final is always true until a streaming ASR exists (§11 Q6).
type Transcript struct {
	Type  string `json:"type"` // "transcript"
	Text  string `json:"text"`
	Final bool   `json:"final"`
}

// AudioBegin announces an incoming TTS stream (§4.4). UtteranceID correlates
// the binary frames, captions, and audio_end/cancel for this stream.
type AudioBegin struct {
	Type                string `json:"type"` // "audio_begin"
	UtteranceID         int    `json:"utterance_id"`
	EstimatedDurationMS int    `json:"estimated_duration_ms,omitempty"`
}

// AudioEnd marks the end of a TTS stream (§4.4). The device leaves the speaking
// state once its playback buffer drains.
type AudioEnd struct {
	Type        string `json:"type"` // "audio_end"
	UtteranceID int    `json:"utterance_id"`
}

// AudioCancel aborts an in-flight TTS stream the server started (§4.4): the
// device flushes its decoder queue for this utterance.
type AudioCancel struct {
	Type        string `json:"type"` // "audio_cancel"
	UtteranceID int    `json:"utterance_id"`
}

// Caption is display text for an utterance (§4.4). Text is cumulative — the
// full caption so far, displayed verbatim. The terminal caption for an utterance
// carries Final=true and OMITS Text (a pure completion marker): the device keeps
// the last text it displayed. Non-terminal captions always carry Text, so the
// omitempty only ever elides the terminal marker's empty string.
type Caption struct {
	Type        string `json:"type"` // "caption"
	UtteranceID int    `json:"utterance_id"`
	Text        string `json:"text,omitempty"`
	Final       bool   `json:"final"`
}

// Display drives the avatar (§4.5), replacing v1's llm{emotion}. Emotion and
// Status are independently optional.
type Display struct {
	Type    string `json:"type"` // "display"
	Emotion string `json:"emotion,omitempty"`
	Status  string `json:"status,omitempty"`
}

// Alert renders a full-screen popup with optional sound (§4.6).
type Alert struct {
	Type    string `json:"type"` // "alert"
	Title   string `json:"title"`
	Message string `json:"message"`
	Emotion string `json:"emotion"`
	Sound   string `json:"sound,omitempty"`
}

// System carries device-level commands (§4.7). Only "reboot" is defined;
// intended for post-OTA, NOT for voice goodbye (use Goodbye + WS close).
type System struct {
	Type    string `json:"type"`    // "system"
	Command string `json:"command"` // "reboot"
}

// ----------------------------------------------------------------------------
// Bidirectional
// ----------------------------------------------------------------------------

// Goodbye is an advisory notification sent immediately before a graceful WS
// close, naming why the session is ending (§4.10). One-way, best-effort: the
// close frame does the real work. Reason ∈ {idle_timeout, user_farewell,
// error, restart, shutdown}; recipients tolerate unknown reasons.
type Goodbye struct {
	Type   string `json:"type"` // "goodbye"
	Reason string `json:"reason,omitempty"`
}

// Error is a standalone, top-level error message (§4.9), either direction.
// Unlike the nested ErrorBody on a response, Code/Message are top-level. RefID
// optionally names the request/utterance the error relates to (0 = absent).
type Error struct {
	Type    string `json:"type"` // "error"
	Code    string `json:"code"`
	Message string `json:"message"`
	RefID   int    `json:"ref_id,omitempty"`
}

// ----------------------------------------------------------------------------
// Tools (both directions) — §6
// ----------------------------------------------------------------------------

// ToolDescriptor describes one callable tool (§6.1). No version field: the
// catalog is rediscovered per session, so the advertised schema is always
// authoritative. Permission ∈ {public, user_only, system_only}.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	ArgsSchema  json.RawMessage `json:"args_schema,omitempty"`
	Permission  string          `json:"permission,omitempty"`
}

// ToolListResult is the payload of a successful tool_list response (§6.2).
type ToolListResult struct {
	Tools      []ToolDescriptor `json:"tools"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

// ToolList is the paginated tool-catalog exchange (§6.2). As a request Cursor
// is set (possibly null); as a response Result or Error is set. The same struct
// serves both directions (§6.5).
type ToolList struct {
	Type   string          `json:"type"` // "tool_list"
	ID     int             `json:"id"`
	Cursor *string         `json:"cursor,omitempty"`
	Result *ToolListResult `json:"result,omitempty"`
	Error  *ErrorBody      `json:"error,omitempty"`
}

// ToolCall invokes a tool (§6.3). As a request Name+Args are set; as a response
// Result (raw JSON: true, an object, etc.) or Error is set.
type ToolCall struct {
	Type   string          `json:"type"` // "tool_call"
	ID     int             `json:"id"`
	Name   string          `json:"name,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrorBody      `json:"error,omitempty"`
}
