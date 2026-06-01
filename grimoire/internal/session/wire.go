// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"io"
)

// This file defines the protocol seam: the voice loop (turn.go, mic.go) talks
// to the device only through these two interfaces and never touches a wire
// type (protocol.* / protov2.*) or the raw socket directly. The v1
// implementations live in wire_v1.go and the v2 implementations in wire_v2.go;
// session.run binds one or the other per the Protocol-Version upgrade header.
// At phase 4 (v2 only — see docs/PROTOCOL_V2.md §10) the v1 implementations and
// the dispatch branch are deleted and the loop is left untouched.

// deviceOut is everything the voice loop sends toward the device. Each method
// maps to whatever the active protocol puts on the wire:
//
//	          v1                              v2
//	Transcript  stt                           transcript
//	Display     llm{emotion} (status dropped) display{emotion,status}
//	SpeakBegin  tts:start                      audio_begin
//	Caption     tts:sentence_start            caption (impl renders cumulative)
//	SpeakPCM    raw Opus binary, real-time     Opus binary, credit-gated
//	            paced (no AEC, fixed buffer)
//	SpeakEnd    tts:stop (after pacing drain)  audio_end
//	Close       WS close                       goodbye + WS close
//
// A reply is one SpeakBegin … SpeakEnd pair, regardless of sentence count —
// the v1 device couples its Speaking state to that pair, so splitting it would
// truncate later sentences (see docs/PROTOCOL_V1.md §5.2). The loop holds the
// pair open for the whole reply; the implementation owns the per-protocol
// audio flow control (v1 pacing vs v2 credits) inside SpeakPCM/SpeakEnd.
//
// Caption takes the new sentence SEGMENT, not cumulative text. v1 emits it
// as-is; v2's impl accumulates segments and renders the cumulative caption
// the spec mandates (§4.4, Q2), emitting the final caption at SpeakEnd. The
// loop stays ignorant of that difference.
type deviceOut interface {
	// Transcript sends an ASR result for display. final=false marks an
	// incremental partial emitted mid-utterance (streaming ASR, §4.3); the
	// authoritative result is sent once with final=true. With streaming off the
	// loop only ever sends the single final=true transcript.
	Transcript(ctx context.Context, text string, final bool) error
	Display(ctx context.Context, emotion, status string) error
	SpeakBegin(ctx context.Context) error
	Caption(ctx context.Context, segment string) error
	SpeakPCM(ctx context.Context, pcm io.Reader) error
	SpeakEnd(ctx context.Context) error
	Close(ctx context.Context, reason string) error
}

// wireDecoder turns an inbound text frame into a normalized inEvent so the
// read loop dispatches on protocol-agnostic events. Binary frames (microphone
// Opus) are raw in both protocols (PROTOCOL_V2 §7) and handled directly in
// onAudioFrame — they don't go through the decoder.
type wireDecoder interface {
	Decode(data []byte) (inEvent, error)
}

// creditSink is the optional extension a deviceOut implements when its protocol
// uses credit-based audio flow control (PROTOCOL_V2 §5). The read loop type-
// asserts s.out to this on an evAudioCredit so the credits reach the in-flight
// SpeakPCM. v1 has no flow control and does not implement it, so v1 sessions
// never produce evAudioCredit and the assertion simply never fires.
type creditSink interface {
	AddCredit(frames int)
}

// errorSink is the optional extension a deviceOut implements when its protocol
// has a first-class error message (PROTOCOL_V2 §4.9/§9). The session reports
// protocol violations and internal failures through it. v1 has no error type
// and does not implement it, so v1 sessions silently degrade (log only) — the
// session's sendError helper makes the difference invisible to the loop.
type errorSink interface {
	SendError(ctx context.Context, code, message string, refID int)
}

// alertSink is the optional extension a deviceOut implements when its protocol
// can render a full-screen device alert (PROTOCOL_V2 §4.6). The session uses it
// to react to inbound telemetry (e.g. battery_low → a "charge me" popup). v1 has
// no alert message and does not implement it, so v1 telemetry is log-only.
type alertSink interface {
	SendAlert(ctx context.Context, title, message, emotion, sound string) error
}

// inEvent is the closed set of normalized device → server events. The v1
// decoder maps protocol.Listen{start/stop/detect} → evListenStart/Stop/Wake,
// collapsing v1's overloaded "listen" type into the same shape v2 sends
// natively (listen_start / listen_stop / wake).
type inEvent interface{ isInEvent() }

// evDupHello is a hello received after the handshake (the loop only warns).
type evDupHello struct{}

// evWake is a wake-word detection (v1: listen{state:detect}; v2: wake).
type evWake struct {
	Phrase string
	Score  float64
}

// evListenStart begins a capture window. Mode is "auto" | "manual" |
// "realtime" | "" (empty = the firmware's AEC-off default, treated as auto).
type evListenStart struct{ Mode string }

// evListenStop ends a capture window (manual mode / device VAD).
type evListenStop struct{}

// evAbort is a barge-in: stop streaming TTS and prepare for a fresh turn.
type evAbort struct{ Reason string }

// evMCP is a v1 MCP-over-WS envelope payload (inner JSON-RPC), routed opaquely
// to the v1 tool port's MCP client.
type evMCP struct{ Payload json.RawMessage }

// evToolResponse is a v2 first-class tool_list/tool_call frame (the whole
// frame, not an envelope), routed to the v2 tool port which correlates it by
// id. v1 never produces it; v2 sends tools as top-level messages, not MCP.
type evToolResponse struct{ Raw []byte }

// evTelemetry is an ambient perception event (v1: event; v2: telemetry).
type evTelemetry struct {
	Name string
	Data json.RawMessage
}

// evAudioCredit refills the server→device audio send budget (v2 only; §5.2).
// The read loop routes its Frames to s.out via the creditSink assertion. v1
// never produces it.
type evAudioCredit struct{ Frames int }

// evGoodbye is the advisory pre-close notification (v2 only; §4.10). The close
// frame still does the real work, so the loop only logs it.
type evGoodbye struct{ Reason string }

// evError is a standalone protocol error from the device (v2 only; §4.9).
// Logged and degraded over, never fatal.
type evError struct {
	Code    string
	Message string
}

// evUnknown is a message whose type is outside the catalog; logged, not fatal.
type evUnknown struct{ Type string }

func (evDupHello) isInEvent()     {}
func (evWake) isInEvent()         {}
func (evListenStart) isInEvent()  {}
func (evListenStop) isInEvent()   {}
func (evAbort) isInEvent()        {}
func (evMCP) isInEvent()          {}
func (evToolResponse) isInEvent() {}
func (evTelemetry) isInEvent()    {}
func (evAudioCredit) isInEvent()  {}
func (evGoodbye) isInEvent()      {}
func (evError) isInEvent()        {}
func (evUnknown) isInEvent()      {}
