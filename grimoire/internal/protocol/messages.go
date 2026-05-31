// SPDX-License-Identifier: AGPL-3.0-or-later

// Package protocol defines the wire-adjacent types shared across the server
// that are not part of the live v2 WebSocket message catalog (that lives in
// package protov2). After the v1 protocol removal this is just two things:
//
//   - AudioParams: the audio-stream descriptor reused by the session config and
//     the v2 hello negotiation.
//   - The OTA HTTP discovery response shapes (OTAResponse + nested types), which
//     the device fetches over plain HTTP on boot to learn its WebSocket URL.
//
// The v1 JSON WebSocket message types (ClientHello/Listen/TTS/etc.) and the v1
// Decode() dispatcher were removed when the server went v2-only — see
// docs/PROTOCOL_V2.md §10 (migration Phase 4). The live wire format is package
// protov2; the OTA handler is package ota.
package protocol

// AudioParams describes one direction of the audio stream (sample rate, frame
// duration). Used by the session config and the OTA response.
type AudioParams struct {
	Format        string `json:"format,omitempty"`         // "opus"
	SampleRate    int    `json:"sample_rate,omitempty"`    // Hz
	Channels      int    `json:"channels,omitempty"`       // 1
	FrameDuration int    `json:"frame_duration,omitempty"` // ms
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
