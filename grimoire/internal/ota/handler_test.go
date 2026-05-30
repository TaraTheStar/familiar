// SPDX-License-Identifier: AGPL-3.0-or-later

package ota

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
)

func TestHandlerGETReturnsWebSocketURL(t *testing.T) {
	h := Handler(Config{
		WebSocketURL:    "ws://test/xiaozhi/v1/",
		FirmwareVersion: "1.3.1",
		NowMillis:       func() (int64, int) { return 1716608284000, -300 },
	}, nil)

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out protocol.OTAResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.WebSocket == nil || out.WebSocket.URL != "ws://test/xiaozhi/v1/" {
		t.Errorf("WebSocket: %#v", out.WebSocket)
	}
	if out.Firmware == nil || out.Firmware.Version != "1.3.1" {
		t.Errorf("Firmware: %#v", out.Firmware)
	}
	if out.ServerTime == nil || out.ServerTime.TimezoneOffset != -300 {
		t.Errorf("ServerTime: %#v", out.ServerTime)
	}
}

func TestHandlerPOSTDrainsBody(t *testing.T) {
	h := Handler(Config{WebSocketURL: "ws://test/"}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Real devices POST a big system-info JSON; we should ignore the body
	// but not 400 on it.
	body := strings.NewReader(`{"version":2,"mac_address":"aa:bb:cc:dd:ee:ff"}`)
	resp, err := http.Post(srv.URL, "application/json", body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)
}

func TestDiscoverHandlerV2Shape(t *testing.T) {
	// With a firmware URL: firmware advertised. Time/token absent (v2 carries
	// the clock in the WS hello, the token in upgrade headers).
	h := DiscoverHandler(Config{
		WebSocketURL:    "ws://test/xiaozhi/",
		FirmwareVersion: "2.0.0",
		FirmwareURL:     "http://test/fw/stack-chan-2.0.0.bin",
		Token:           "secret", // must NOT leak into the v2 discover body
	}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["ws_url"] != "ws://test/xiaozhi/" {
		t.Errorf("ws_url = %v", out["ws_url"])
	}
	// Leaner than v1: no server_time, no token.
	if _, ok := out["server_time"]; ok {
		t.Error("v2 discover must not include server_time")
	}
	if _, ok := out["token"]; ok {
		t.Error("v2 discover must not include token")
	}
	fw, ok := out["firmware"].(map[string]any)
	if !ok || fw["version"] != "2.0.0" || fw["url"] != "http://test/fw/stack-chan-2.0.0.bin" {
		t.Errorf("firmware = %v", out["firmware"])
	}
}

func TestDiscoverHandlerNoFirmwareWhenURLEmpty(t *testing.T) {
	// No firmware URL → no firmware block (device never sees a phantom update).
	h := DiscoverHandler(Config{WebSocketURL: "ws://test/xiaozhi/", FirmwareVersion: "2.0.0"}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := out["firmware"]; ok {
		t.Errorf("firmware should be omitted when no URL configured: %v", out["firmware"])
	}
}

func TestHandlerRejectsPUT(t *testing.T) {
	h := Handler(Config{WebSocketURL: "ws://test/"}, nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest("PUT", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT status=%d, want 405", resp.StatusCode)
	}
}
