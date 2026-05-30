// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func discardV2Out(initialCredit int) *v2Out {
	return newV2Out(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), initialCredit)
}

// TestV2CreditBackpressure proves the flow-control gate: once the initial
// budget is spent, acquireCredit blocks until AddCredit replenishes it (§5).
// This is the mechanism that makes ESP32 decoder-buffer overruns impossible.
func TestV2CreditBackpressure(t *testing.T) {
	o := discardV2Out(2)
	ctx := context.Background()

	// The two initial credits are available immediately.
	for i := 0; i < 2; i++ {
		if err := o.acquireCredit(ctx); err != nil {
			t.Fatalf("initial credit %d: %v", i, err)
		}
	}

	// The third must block — no credit left.
	done := make(chan error, 1)
	go func() { done <- o.acquireCredit(ctx) }()
	select {
	case <-done:
		t.Fatal("acquireCredit returned with zero credit; should have blocked")
	case <-time.After(50 * time.Millisecond):
	}

	// A refill unblocks it.
	o.AddCredit(1)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("acquireCredit after refill: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("acquireCredit did not unblock after AddCredit")
	}
}

// TestV2CreditContextCancel proves a blocked SpeakPCM unwinds on turn
// cancellation (e.g. barge-in / WS close) rather than hanging.
func TestV2CreditContextCancel(t *testing.T) {
	o := discardV2Out(0) // no credit at all → first acquire blocks
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- o.acquireCredit(ctx) }()
	select {
	case <-done:
		t.Fatal("acquireCredit returned with zero credit; should have blocked")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("acquireCredit should return the context error on cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("acquireCredit did not return after context cancel")
	}
}

func TestGoodbyeReason(t *testing.T) {
	cases := map[string]string{
		"goodbye":      "user_farewell", // the loop's exit-phrase reason
		"idle_timeout": "idle_timeout",  // already a spec reason: passthrough
		"":             "",
	}
	for in, want := range cases {
		if got := goodbyeReason(in); got != want {
			t.Errorf("goodbyeReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClockSync(t *testing.T) {
	// A fixed -5h zone must surface as tz_offset_min = -300, and unix_ms must be
	// a real, recent wall-clock value (the goldens normalize it to 0, so this is
	// where the live value is actually checked).
	s := &Session{cfg: Config{TimeLocation: time.FixedZone("test-5h", -5*3600)}}
	before := time.Now().UnixMilli()
	ts := s.clockSync()
	after := time.Now().UnixMilli()

	if ts.TZOffsetMin != -300 {
		t.Errorf("tz_offset_min = %d, want -300", ts.TZOffsetMin)
	}
	if ts.UnixMS < before || ts.UnixMS > after {
		t.Errorf("unix_ms = %d, want within [%d,%d]", ts.UnixMS, before, after)
	}

	// nil TimeLocation falls back to UTC → offset 0, machine-independent.
	if got := (&Session{}).clockSync().TZOffsetMin; got != 0 {
		t.Errorf("nil TimeLocation tz_offset_min = %d, want 0 (UTC)", got)
	}
}

func TestParseProtocolVersion(t *testing.T) {
	cases := []struct {
		header  string
		wantVer int
		wantOK  bool
	}{
		{"", 1, true}, // omitted header defaults to v1
		{"1", 1, true},
		{"2", 2, true},
		{" 2 ", 2, true}, // tolerate surrounding whitespace
		{"3", 0, false},
		{"v2", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		gotVer, gotOK := parseProtocolVersion(c.header)
		if gotVer != c.wantVer || gotOK != c.wantOK {
			t.Errorf("parseProtocolVersion(%q) = (%d,%v), want (%d,%v)", c.header, gotVer, gotOK, c.wantVer, c.wantOK)
		}
	}
}

// TestUpgradeRejectsUnknownVersion proves the dispatcher fails closed: an
// unsupported Protocol-Version is rejected with HTTP 426 before the WebSocket
// upgrade, rather than upgrading into a protocol the server can't speak.
func TestUpgradeRejectsUnknownVersion(t *testing.T) {
	srv := httptest.NewServer(Handler(Config{HandshakeTimeout: time.Second}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Protocol-Version": []string{"3"}},
	})
	if err == nil {
		t.Fatal("Dial succeeded; expected rejection for Protocol-Version: 3")
	}
	if resp == nil {
		t.Fatalf("no HTTP response on rejected upgrade: %v", err)
	}
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("status = %d, want %d (426 Upgrade Required)", resp.StatusCode, http.StatusUpgradeRequired)
	}
}
