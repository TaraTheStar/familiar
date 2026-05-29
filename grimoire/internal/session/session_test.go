// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
)

func TestHandshakeRoundtrip(t *testing.T) {
	srv := httptest.NewServer(Handler(Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	// Send ClientHello.
	hello := protocol.ClientHello{
		Type: "hello", Version: 1, Transport: "websocket",
		Features:    &protocol.Features{MCP: true},
		AudioParams: &protocol.AudioParams{Format: "opus", SampleRate: 16000, Channels: 1, FrameDuration: 60},
	}
	helloBytes, _ := json.Marshal(hello)
	if err := conn.Write(ctx, websocket.MessageText, helloBytes); err != nil {
		t.Fatalf("Write hello: %v", err)
	}

	// Read ServerHello.
	mt, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if mt != websocket.MessageText {
		t.Fatalf("unexpected message type %v", mt)
	}

	var resp protocol.ServerHello
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decode response: %v (raw=%s)", err, data)
	}
	if resp.Type != "hello" || resp.Transport != "websocket" {
		t.Errorf("ServerHello envelope wrong: %+v", resp)
	}
	if resp.SessionID == "" {
		t.Errorf("ServerHello.SessionID empty")
	}
	if resp.AudioParams == nil || resp.AudioParams.SampleRate != 24000 {
		t.Errorf("ServerHello.AudioParams wrong: %+v", resp.AudioParams)
	}
}

func TestHandshakeRejectsNonHelloFirstMessage(t *testing.T) {
	srv := httptest.NewServer(Handler(Config{
		HandshakeTimeout: 2 * time.Second,
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Send a Listen instead of a hello. Server should close.
	bad, _ := json.Marshal(protocol.Listen{Type: "listen", State: "start", Mode: "auto"})
	if err := conn.Write(ctx, websocket.MessageText, bad); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Subsequent Read should fail with a close.
	_, _, err = conn.Read(ctx)
	if err == nil {
		t.Fatal("expected close after bad first message")
	}
	if cs := websocket.CloseStatus(err); cs == -1 {
		t.Errorf("expected websocket close status, got %v", err)
	}
}

func TestHandshakeTimeout(t *testing.T) {
	srv := httptest.NewServer(Handler(Config{
		HandshakeTimeout: 100 * time.Millisecond,
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Don't send anything; server should time out and close.
	_, _, err = conn.Read(ctx)
	if err == nil {
		t.Fatal("expected close on handshake timeout")
	}
}
