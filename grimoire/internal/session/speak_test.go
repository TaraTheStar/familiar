// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// TestSpeakHardcodedReplyOverWS spins up:
//   - a fake Kokoro HTTP server that returns synthetic PCM
//   - a stackend WS server configured to speak HardcodedReply on listen:stop
//   - a WS client that sends ClientHello + listen:start + listen:stop
//
// Then asserts the wire shape: tts:start → tts:sentence_start{text} →
// N binary Opus frames → tts:stop. This is the full milestone-2 flow
// minus the actual device.
func TestSpeakHardcodedReplyOverWS(t *testing.T) {
	kokoro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return ~300ms of synthetic PCM at 24kHz (= 7200 samples = 14400 bytes).
		// Real kokoro takes much longer but produces the same format.
		var body struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Input == "" {
			http.Error(w, "missing input", 400)
			return
		}
		pcm := synthesizePCM(7200, 24000, 440)
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(pcm)
	}))
	defer kokoro.Close()

	srv := httptest.NewServer(Handler(Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
		HardcodedReply:   "Hello, I heard you.",
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	// ClientHello
	hello, _ := json.Marshal(protocol.ClientHello{
		Type: "hello", Version: 1, Transport: "websocket",
		AudioParams: &protocol.AudioParams{Format: "opus", SampleRate: 16000, Channels: 1, FrameDuration: 60},
	})
	mustWriteText(t, ctx, conn, hello)

	// Read ServerHello + discard
	mustReadText(t, ctx, conn)

	// listen:start
	mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "start", Mode: "auto"}))
	// listen:stop — should trigger the hardcoded reply
	mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "stop"}))

	// Expect tts:start (skipping any incidental llm/emotion frames).
	var got protocol.TTS
	for {
		mustReadJSON(t, ctx, conn, &got)
		if got.Type == "tts" {
			break
		}
		// Tolerate llm/emotion frames sent by setEmotion.
	}
	if got.State != "start" {
		t.Fatalf("first tts frame: got %+v, want tts:start", got)
	}

	// Expect tts:sentence_start with our hardcoded text
	mustReadJSON(t, ctx, conn, &got)
	if got.State != "sentence_start" || got.Text != "Hello, I heard you." {
		t.Fatalf("frame 2: got %+v, want sentence_start with hardcoded reply", got)
	}

	// Expect binary frames until tts:stop. 7200 samples / 1440 per frame = 5 frames.
	frameCount := 0
	for {
		mt, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read frame %d: %v", frameCount+2, err)
		}
		if mt == websocket.MessageBinary {
			if len(data) == 0 {
				t.Errorf("empty Opus frame")
			}
			frameCount++
			continue
		}
		// text frame; should be tts:stop
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("decode final frame: %v", err)
		}
		if got.State != "stop" {
			t.Errorf("expected tts:stop, got %+v", got)
		}
		break
	}

	// Sanity: 7200 PCM samples at 1440 per frame = exactly 5 frames.
	if frameCount != 5 {
		t.Errorf("opus frames=%d, want 5 (= 300ms at 60ms/frame)", frameCount)
	}
}

// Helpers --------------------------------------------------------------------

func synthesizePCM(samples, sampleRate, freqHz int) []byte {
	out := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		t := float64(i) / float64(sampleRate)
		v := int16(8000 * math.Sin(2*math.Pi*float64(freqHz)*t))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

func mustWriteText(t *testing.T, ctx context.Context, c *websocket.Conn, data []byte) {
	t.Helper()
	if err := c.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func mustReadText(t *testing.T, ctx context.Context, c *websocket.Conn) []byte {
	t.Helper()
	mt, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if mt != websocket.MessageText {
		t.Fatalf("Read: expected text, got binary (%d bytes)", len(data))
	}
	return data
}

func mustReadJSON(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	data := mustReadText(t, ctx, c)
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("Unmarshal %s: %v", data, err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

// Avoid unused-import in case io is referenced only by other tests later.
var _ = io.EOF
