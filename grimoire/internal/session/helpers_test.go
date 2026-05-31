// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/coder/websocket"
)

// Shared test helpers. These were relocated here from the v1 test files
// (turn_test.go, speak_test.go) when the v1 protocol was removed, so the
// surviving v2 tests that depend on them keep compiling.

// fakeASR is a stand-in for whisper.cpp; lets tests run without the real model.
type fakeASR struct {
	out string
	err error
}

func (f *fakeASR) Transcribe(pcm []int16) (string, error) { return f.out, f.err }

// sendFakeAudioFrame writes one valid Opus mic frame over the connection.
func sendFakeAudioFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	// Constructed via the encoder so we know it's a valid Opus packet
	// the server will decode without error.
	enc := newSendEncoder(t)
	pcm := make([]int16, enc.SamplesPerFrame())
	for i := range pcm {
		pcm[i] = int16(i % 1000)
	}
	pkt, err := enc.Encode(pcm)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, pkt); err != nil {
		t.Fatalf("write audio frame: %v", err)
	}
}

func mustWriteText(t *testing.T, ctx context.Context, c *websocket.Conn, data []byte) {
	t.Helper()
	if err := c.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func mustReadText(t *testing.T, ctx context.Context, c *websocket.Conn) []byte {
	t.Helper()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ == websocket.MessageText {
			return data
		}
	}
}

func mustReadFrame(t *testing.T, ctx context.Context, c *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return typ, data
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
