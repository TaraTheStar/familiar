// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ota implements the device's HTTP discovery / OTA endpoint.
//
// The device hits this on every boot (the URL is flashed into NVS as
// CONFIG_OTA_URL). Despite the name, the practical purpose is handing back
// the WebSocket URL the device should use for the live session. Firmware
// version is returned at-current to suppress the OTA upgrade path; we only
// flip that to trigger a deliberate device reflash.
package ota

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
)

// Config is the static response template. We don't actually read the
// device's POST body — it's just system info for inventory which we don't
// care about right now.
type Config struct {
	// WebSocketURL is the URL the device will open for the live session,
	// e.g. "ws://192.0.2.10:9098/grimoire/".
	WebSocketURL string

	// FirmwareVersion is echoed back as the canonical version. Set to the
	// device's current version to suppress OTA; bump to force a reflash.
	FirmwareVersion string

	// FirmwareURL is only consulted by the device if FirmwareVersion >
	// current. Can be empty when we don't run an OTA server.
	FirmwareURL string

	// Token, if non-empty, is included in the response so the device puts
	// it in Authorization: Bearer headers when opening the WS.
	Token string

	// NowMillis returns the current time in unix ms, with a TZ offset in
	// signed minutes from UTC. Both passed back to set the device clock.
	// Defaults to time.Now / local zone if nil.
	NowMillis func() (millis int64, tzOffsetMin int)
}

// Handler returns an http.HandlerFunc that serves the OTA discovery
// response. Logs each request at info level via the provided logger.
func Handler(cfg Config, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Device sends POST with a system-info body or a GET with no body.
		// Both are valid; we don't inspect the body.
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Drain + discard body so the device's TCP send doesn't stall.
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}

		ms, tz := nowDefault, 0
		if cfg.NowMillis != nil {
			ms, tz = cfg.NowMillis()
		}

		resp := protocol.OTAResponse{
			WebSocket: &protocol.WebSocketConfig{
				URL:   cfg.WebSocketURL,
				Token: cfg.Token,
			},
			ServerTime: &protocol.ServerTime{
				Timestamp:      ms,
				TimezoneOffset: tz,
			},
			Firmware: &protocol.Firmware{
				Version: cfg.FirmwareVersion,
				URL:     cfg.FirmwareURL,
			},
		}

		logger.Info("ota discovery",
			"remote", r.RemoteAddr,
			"device_id", r.Header.Get("Device-Id"),
			"client_id", r.Header.Get("Client-Id"),
			"user_agent", r.Header.Get("User-Agent"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// nowDefault is replaced with time.Now().UnixMilli() inside the function
// when cfg.NowMillis is nil; this var exists so tests can avoid time.
var nowDefault int64 = 0

// DiscoverResponse is the Protocol v2 discovery payload (PROTOCOL_V2 §2.1). It
// is deliberately leaner than the v1 OTA response: no server_time (folded into
// the WS hello), no activation/mqtt. Just where to connect and an optional
// firmware update.
type DiscoverResponse struct {
	WSURL    string            `json:"ws_url"`
	Firmware *DiscoverFirmware `json:"firmware,omitempty"`
}

// DiscoverFirmware names an available firmware image; the device upgrades only
// if Version is newer than what it runs.
type DiscoverFirmware struct {
	Version string `json:"version"`
	URL     string `json:"url,omitempty"`
}

// DiscoverHandler serves the v2 `/discover` endpoint. It reuses the OTA Config
// (WebSocketURL + firmware fields) but emits the v2 shape. Time and token are
// not included — v2 carries the clock in the WS hello and the token in the
// upgrade headers. Firmware is advertised only when a URL is configured.
func DiscoverHandler(cfg Config, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}

		resp := DiscoverResponse{WSURL: cfg.WebSocketURL}
		if cfg.FirmwareURL != "" {
			resp.Firmware = &DiscoverFirmware{Version: cfg.FirmwareVersion, URL: cfg.FirmwareURL}
		}

		logger.Info("v2 discovery",
			"remote", r.RemoteAddr,
			"device_id", r.Header.Get("Device-Id"),
			"client_id", r.Header.Get("Client-Id"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}
