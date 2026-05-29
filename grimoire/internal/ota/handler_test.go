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
