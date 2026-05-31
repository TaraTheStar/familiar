// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// After the v1 protocol removal this package holds only AudioParams and the OTA
// HTTP discovery response shapes (the v1 WebSocket message types and the Decode
// dispatcher are gone — the live wire format is package protov2). These tests
// pin the JSON shape of what survives.

func TestEncodeOTAResponse(t *testing.T) {
	resp := OTAResponse{
		WebSocket:  &WebSocketConfig{URL: "ws://192.0.2.10:9098/grimoire/"},
		ServerTime: &ServerTime{Timestamp: 1716608284000, TimezoneOffset: -300},
		Firmware:   &Firmware{Version: "1.3.1", URL: "http://example/x.bin"},
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var roundtrip OTAResponse
	if err := json.Unmarshal(out, &roundtrip); err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	if roundtrip.WebSocket == nil || roundtrip.WebSocket.URL != resp.WebSocket.URL {
		t.Errorf("URL roundtrip mismatch: %#v", roundtrip.WebSocket)
	}
	if roundtrip.ServerTime == nil || roundtrip.ServerTime.TimezoneOffset != -300 {
		t.Errorf("TZ roundtrip mismatch: %#v", roundtrip.ServerTime)
	}
	if roundtrip.Firmware == nil || roundtrip.Firmware.Version != "1.3.1" {
		t.Errorf("Firmware roundtrip mismatch: %#v", roundtrip.Firmware)
	}
}

// TestOTAResponseOmitsEmpty confirms the omitempty pointers drop out when nil,
// so a minimal OTA reply (just the WebSocket URL) stays lean.
func TestOTAResponseOmitsEmpty(t *testing.T) {
	out, err := json.Marshal(OTAResponse{WebSocket: &WebSocketConfig{URL: "ws://x/grimoire/"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(out)
	for _, banned := range []string{"server_time", "firmware"} {
		if strings.Contains(s, banned) {
			t.Errorf("expected %q to be omitted, got %s", banned, s)
		}
	}
}
