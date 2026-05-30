// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
)

// readFrame reads text frames until one of wantTypes arrives, returning its type
// and raw bytes. Binary and other text frames are skipped.
func readFrame(t *testing.T, ctx context.Context, conn *websocket.Conn, wantTypes ...string) (string, []byte) {
	t.Helper()
	want := map[string]bool{}
	for _, w := range wantTypes {
		want[w] = true
	}
	for {
		mt, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read (waiting for %v): %v", wantTypes, err)
		}
		if mt != websocket.MessageText {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			continue
		}
		if want[head.Type] {
			return head.Type, data
		}
	}
}

// TestV2TelemetryBatteryLowAlert proves telemetry is wired, not just logged: a
// battery_low event drives a full-screen alert back to the device (§4.8 → §4.6),
// with the percent surfaced in the message.
func TestV2TelemetryBatteryLowAlert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv, conn := dialV2(t, ctx, Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
	})
	defer srv.Close()
	defer conn.CloseNow()

	mustWriteText(t, ctx, conn, mustJSON(t, protov2.Telemetry{
		Type:  "telemetry",
		Event: "battery_low",
		Data:  json.RawMessage(`{"percent":7}`),
	}))

	mt, data := readFrame(t, ctx, conn, "alert")
	if mt != "alert" {
		t.Fatalf("expected alert, got %q", mt)
	}
	var a protov2.Alert
	if err := json.Unmarshal(data, &a); err != nil {
		t.Fatalf("decode alert: %v", err)
	}
	if a.Title != "Battery low" {
		t.Errorf("alert title = %q", a.Title)
	}
	if a.Emotion != "sad" {
		t.Errorf("alert emotion = %q", a.Emotion)
	}
	if want := "Battery at 7% — please charge me"; a.Message != want {
		t.Errorf("alert message = %q, want %q", a.Message, want)
	}
}

// TestV2TelemetryUnknownNoReply proves an unrecognized telemetry event is
// tolerated silently (logged, no protocol error, no spurious frame).
func TestV2TelemetryUnknownNoReply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv, conn := dialV2(t, ctx, Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
	})
	defer srv.Close()
	defer conn.CloseNow()

	mustWriteText(t, ctx, conn, mustJSON(t, protov2.Telemetry{
		Type: "telemetry", Event: "face_seen", Data: json.RawMessage(`{"x":1}`),
	}))

	// Send a battery_low next; the FIRST frame we get back must be the alert for
	// it — proving face_seen produced nothing ahead of it.
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.Telemetry{
		Type: "telemetry", Event: "battery_low",
	}))
	mt, _ := readFrame(t, ctx, conn, "alert")
	if mt != "alert" {
		t.Fatalf("first reply after unknown+battery_low was %q, want alert", mt)
	}
}
