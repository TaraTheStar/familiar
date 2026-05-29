// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// fakeASR is a stand-in for whisper.cpp; lets the turn test run without
// loading the real model.
type fakeASR struct {
	out string
	err error
}

func (f *fakeASR) Transcribe(pcm []int16) (string, error) { return f.out, f.err }

// TestHandleTurnEndToEnd drives the full voice loop:
//
//	mic frames → mock ASR → mock LLM stream → mock Kokoro → wire frames
//
// Asserts the on-wire shape:
//
//	stt → tts:start → sentence_start("Sentence one.") → opus frames → tts:stop
//	  → tts:start → sentence_start("Sentence two!") → opus frames → tts:stop
//	  → tts:start → sentence_start("And a final fragment") → opus frames → tts:stop
//
// Confirms streaming TTS: each sentence is spoken as it falls out of the
// LLM stream rather than waiting for the whole response.
func TestHandleTurnEndToEnd(t *testing.T) {
	// Fake Kokoro — returns ~120ms of synthetic 24kHz PCM per sentence.
	kokoro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "audio/pcm")
		// 2880 samples = 120ms at 24kHz = 2 Opus frames at 60ms each.
		pcm := make([]byte, 2880*2)
		_, _ = w.Write(pcm)
	}))
	defer kokoro.Close()

	// Fake LLM — streams a 3-sentence reply.
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(content string) {
			ev := map[string]any{
				"choices": []map[string]any{
					{"delta": map[string]any{"content": content}},
				},
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		// Stream the response as it would arrive from a real LLM, in
		// awkwardly-chunked deltas.
		write("Sentence")
		write(" one. ")
		write("Sentence two")
		write("! ")
		write("And a final ")
		write("fragment")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer llmServer.Close()

	// Server config.
	srv := httptest.NewServer(Handler(Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		MCPInitTimeout:   200 * time.Millisecond,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
		ASR:              &fakeASR{out: "what time is it"},
		LLM:              &llm.Client{BaseURL: llmServer.URL, Model: "test-model"},
		SystemPrompt:     "You are a test robot.",
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	// Hello
	mustWriteText(t, ctx, conn, mustJSON(t, protocol.ClientHello{
		Type: "hello", Version: 1, Transport: "websocket",
		AudioParams: &protocol.AudioParams{Format: "opus", SampleRate: 16000, Channels: 1, FrameDuration: 60},
	}))
	mustReadText(t, ctx, conn)

	mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "start", Mode: "auto"}))
	sendFakeAudioFrame(t, ctx, conn)
	mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "stop"}))

	// Expected wire shape, in order — ONE Speaking session for the whole
	// reply (so consecutive sentences play gaplessly and the device never
	// drops to Listening mid-reply, which would clip later sentences):
	//   stt{text:"what time is it"}
	//   tts:start
	//     sentence_start("Sentence one."),        N opus frames
	//     sentence_start("Sentence two!"),         M opus frames
	//     sentence_start("And a final fragment"),  K opus frames
	//   tts:stop
	gotSentences := []string{}
	sttSeen := false
	currentState := ""
	binaryFramesThisTurn := 0

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && len(gotSentences) < 3 {
		mt, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read: %v (sentences so far: %v)", err, gotSentences)
		}
		switch mt {
		case websocket.MessageBinary:
			binaryFramesThisTurn++
		case websocket.MessageText:
			head := struct {
				Type, State, Text string
			}{}
			if err := json.Unmarshal(data, &head); err != nil {
				t.Fatalf("bad text frame: %v", err)
			}
			switch head.Type {
			case "stt":
				if head.Text != "what time is it" {
					t.Errorf("stt text=%q", head.Text)
				}
				sttSeen = true
			case "tts":
				switch head.State {
				case "start":
					currentState = "start"
					binaryFramesThisTurn = 0
				case "sentence_start":
					// Valid anywhere inside an open session (after start or
					// after a previous sentence_start) — not its own session.
					if currentState != "start" && currentState != "sentence_start" {
						t.Errorf("sentence_start outside an open tts session")
					}
					gotSentences = append(gotSentences, head.Text)
					currentState = "sentence_start"
				case "stop":
					if binaryFramesThisTurn == 0 {
						t.Errorf("tts:stop without any opus frames")
					}
					currentState = ""
				}
			}
		}
	}

	if !sttSeen {
		t.Error("no stt frame received")
	}
	wantSentences := []string{"Sentence one.", "Sentence two!", "And a final fragment"}
	if len(gotSentences) != len(wantSentences) {
		t.Fatalf("got %d sentences (%v), want %d (%v)",
			len(gotSentences), gotSentences, len(wantSentences), wantSentences)
	}
	for i := range wantSentences {
		if gotSentences[i] != wantSentences[i] {
			t.Errorf("sentence[%d]=%q, want %q", i, gotSentences[i], wantSentences[i])
		}
	}
}

// TestHandleTurnEmptyTranscriptIsSkipped: fakeASR returns "", which
// should cause handleTurn to bail without calling LLM or TTS.
func TestHandleTurnEmptyTranscriptIsSkipped(t *testing.T) {
	llmCalled := false
	llmServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		llmCalled = true
	}))
	defer llmServer.Close()

	kokoroCalled := false
	kokoro := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		kokoroCalled = true
	}))
	defer kokoro.Close()

	srv := httptest.NewServer(Handler(Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		MCPInitTimeout:   200 * time.Millisecond,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
		ASR:              &fakeASR{out: ""},
		LLM:              &llm.Client{BaseURL: llmServer.URL, Model: "test"},
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

	mustWriteText(t, ctx, conn, mustJSON(t, protocol.ClientHello{
		Type: "hello", Version: 1, Transport: "websocket",
		AudioParams: &protocol.AudioParams{Format: "opus", SampleRate: 16000, Channels: 1, FrameDuration: 60},
	}))
	mustReadText(t, ctx, conn)

	mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "start"}))
	// Send a real Opus frame so micBuf is non-empty.
	sendFakeAudioFrame(t, ctx, conn)
	mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "stop"}))

	// Wait a beat for the turn goroutine to run + decide to skip.
	time.Sleep(200 * time.Millisecond)

	if llmCalled {
		t.Error("LLM was called despite empty transcript")
	}
	if kokoroCalled {
		t.Error("Kokoro was called despite empty transcript")
	}
}

// sendFakeAudioFrame writes one Opus-encoded frame of synthetic audio so
// micBuf has at least one frame's worth of PCM when listen:stop arrives.
func sendFakeAudioFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	// Constructed via the encoder so we know it's a valid Opus packet
	// the server will decode without error.
	enc := newSendEncoder(t)
	pcm := make([]int16, enc.SamplesPerFrame())
	for i := range pcm {
		pcm[i] = int16(i % 1000)
	}
	opusBytes, err := enc.Encode(pcm)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out := append([]byte(nil), opusBytes...)
	if err := conn.Write(ctx, websocket.MessageBinary, out); err != nil {
		t.Fatalf("Write binary: %v", err)
	}
}

// bytesEq avoids importing reflect for a trivial slice compare.
func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ = bytesEq
var _ = binary.LittleEndian
