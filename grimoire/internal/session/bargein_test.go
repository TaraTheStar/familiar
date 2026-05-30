// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// dialV2 opens a v2 session and completes the hello, returning the connection.
func dialV2(t *testing.T, ctx context.Context, cfg Config) (*httptest.Server, *websocket.Conn) {
	t.Helper()
	srv := httptest.NewServer(Handler(cfg))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Protocol-Version": []string{"2"}},
	})
	if err != nil {
		srv.Close()
		t.Fatalf("Dial: %v", err)
	}
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ClientHello{
		Type: "hello", ID: 1,
		Client: protov2.ClientInfo{Name: "stack-chan"},
		Audio: protov2.AudioConfig{
			In:  protov2.AudioStream{Codec: "opus", Rate: 16000, Channels: 1, FrameMS: 60},
			Out: protov2.AudioStream{Codec: "opus", Rate: 24000, Channels: 1, FrameMS: 60},
		},
	}))
	mustReadText(t, ctx, conn) // server hello
	return srv, conn
}

// readUntilType reads text frames until one of wantTypes arrives, returning its
// decoded {type, code} view. Binary frames and other text types are skipped.
func readUntilType(t *testing.T, ctx context.Context, conn *websocket.Conn, wantTypes ...string) (typ, code string) {
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
			Code string `json:"code"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			continue
		}
		if want[head.Type] {
			return head.Type, head.Code
		}
	}
}

// TestV2BargeIn proves barge-in: with the audio stream blocked on flow control,
// an abort cancels the in-flight reply and the server emits audio_cancel (not
// audio_end), telling the device to flush its decoder queue (§4.4/§8).
func TestV2BargeIn(t *testing.T) {
	kokoro := constantPCMKokoro(t, 2880) // 2 frames/sentence
	defer kokoro.Close()
	// Several sentences so there is plenty of audio left to interrupt.
	llmServer := sentenceStreamLLM(t, "Hello there. ", "How are you", "? ", "Nice day", ".")
	defer llmServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv, conn := dialV2(t, ctx, Config{
		TTSAudio:           protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:           protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout:   2 * time.Second,
		MCPInitTimeout:     200 * time.Millisecond,
		AudioCreditInitial: 1, // 1 frame then the stream blocks awaiting credit we never send
		Kokoro:             &tts.KokoroClient{BaseURL: kokoro.URL},
		ASR:                &fakeASR{out: "tell me a story"},
		LLM:                &llm.Client{BaseURL: llmServer.URL, Model: "test"},
	})
	defer srv.Close()
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Drive a turn; the reply starts streaming and stalls on credit.
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStart{Type: "listen_start", Mode: "auto"}))
	sendFakeAudioFrame(t, ctx, conn)
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStop{Type: "listen_stop"}))

	// Wait until TTS has actually begun, then barge in.
	if typ, _ := readUntilType(t, ctx, conn, "audio_begin"); typ != "audio_begin" {
		t.Fatalf("expected audio_begin, got %q", typ)
	}
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.Abort{Type: "abort", Reason: "wake"}))

	// The stream must be aborted, not ended.
	typ, _ := readUntilType(t, ctx, conn, "audio_cancel", "audio_end")
	if typ != "audio_cancel" {
		t.Errorf("after abort: got %q, want audio_cancel", typ)
	}
}

// TestV2ProtocolViolationError proves a malformed frame draws a first-class
// error{PROTOCOL_VIOLATION} and the session stays up (§9.4). No LLM is
// configured, so there is no tool discovery to interleave.
func TestV2ProtocolViolationError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv, conn := dialV2(t, ctx, Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
	})
	defer srv.Close()
	defer conn.Close(websocket.StatusNormalClosure, "")

	if err := conn.Write(ctx, websocket.MessageText, []byte("{not valid json")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	typ, code := readUntilType(t, ctx, conn, "error")
	if typ != "error" || code != "PROTOCOL_VIOLATION" {
		t.Errorf("got type=%q code=%q, want error/PROTOCOL_VIOLATION", typ, code)
	}
}

// TestV2LLMFailedError proves an internal LLM failure surfaces as
// error{LLM_FAILED} (§9.2/§9.3) rather than silent degradation.
func TestV2LLMFailedError(t *testing.T) {
	// LLM endpoint always 500s.
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer llmServer.Close()
	kokoro := constantPCMKokoro(t, 1440)
	defer kokoro.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv, conn := dialV2(t, ctx, Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		MCPInitTimeout:   200 * time.Millisecond,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
		ASR:              &fakeASR{out: "what time is it"},
		LLM:              &llm.Client{BaseURL: llmServer.URL, Model: "test"},
	})
	defer srv.Close()
	defer conn.Close(websocket.StatusNormalClosure, "")

	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStart{Type: "listen_start", Mode: "auto"}))
	sendFakeAudioFrame(t, ctx, conn)
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStop{Type: "listen_stop"}))

	_, code := readUntilType(t, ctx, conn, "error")
	if code != "LLM_FAILED" {
		t.Errorf("got error code=%q, want LLM_FAILED", code)
	}
}
