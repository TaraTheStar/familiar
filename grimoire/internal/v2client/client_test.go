// SPDX-License-Identifier: AGPL-3.0-or-later

package v2client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
	"github.com/TaraTheStar/familiar/grimoire/internal/session"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
	"github.com/TaraTheStar/familiar/grimoire/internal/v2client"
)

// These tests drive the REAL reference client against the REAL v2 server over a
// loopback WebSocket. They are the end-to-end proof for Stage A: the full
// handshake, Opus interop in both directions, credit flow, captions, tools, and
// barge-in — all exercised through the same code paths a device would, just
// with fake ASR/LLM/TTS backends so it runs on a laptop in milliseconds.

type fakeASR struct{ out string }

func (f fakeASR) Transcribe(_ []int16) (string, error) { return f.out, nil }

// constantPCMKokoro returns a fake Kokoro that emits `samples` of 24kHz silence
// per request (s16). 1440 samples = one 60ms Opus frame.
func constantPCMKokoro(samples int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(make([]byte, samples*2))
	}))
}

// contentStreamLLM streams the given content deltas then [DONE].
func contentStreamLLM(deltas ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, d := range deltas {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", d)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// micTone synthesises `ms` of a 440Hz sine at 16kHz mono s16 — realistic mic
// energy so nothing upstream skips an apparently-silent turn. The fake ASR
// ignores the samples; the bytes just have to flow.
func micTone(ms int) []byte {
	const rate = 16000
	n := rate * ms / 1000
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		pcm[i*2] = byte(v)
		pcm[i*2+1] = byte(v >> 8)
	}
	return pcm
}

func wsURL(t *testing.T, cfg session.Config) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(session.Handler(cfg))
	return "ws" + strings.TrimPrefix(srv.URL, "http"), srv.Close
}

func baseConfig(kokoroURL, llmURL, transcript string) session.Config {
	return session.Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		MCPInitTimeout:   300 * time.Millisecond,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoroURL},
		ASR:              fakeASR{out: transcript},
		LLM:              &llm.Client{BaseURL: llmURL, Model: "test-model"},
	}
}

// TestRunMultiSentence is the headline Stage-A proof: a full v2 turn through the
// real client renders a transcript, cumulative captions, and decodable TTS.
func TestRunMultiSentence(t *testing.T) {
	kokoro := constantPCMKokoro(2880) // 2 frames per sentence
	defer kokoro.Close()
	llmServer := contentStreamLLM("Hello", " there. ", "How are", " you?")
	defer llmServer.Close()

	url, closeSrv := wsURL(t, baseConfig(kokoro.URL, llmServer.URL, "what time is it"))
	defer closeSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := v2client.Run(ctx, v2client.Config{
		URL:    url,
		MicPCM: micTone(300),
		Logf:   t.Logf,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.ServerHello.Server == nil || res.ServerHello.Server.Name == "" {
		t.Error("server hello missing server identity")
	}
	if res.ServerHello.Time == nil {
		t.Error("server hello missing clock sync")
	}
	if res.ServerHello.FlowControl == nil || res.ServerHello.FlowControl.AudioCreditInitial == 0 {
		t.Error("server hello missing flow control credit")
	}
	if res.Transcript != "what time is it" {
		t.Errorf("transcript = %q", res.Transcript)
	}
	if res.FinalCaption != "Hello there. How are you?" {
		t.Errorf("final caption = %q, want %q", res.FinalCaption, "Hello there. How are you?")
	}
	if res.AudioFrames == 0 {
		t.Error("received no TTS audio frames")
	}
	// Two sentences × 2 frames each = 4 decoded frames → matching PCM length.
	if want := res.AudioFrames * 1440 * 2; len(res.AudioPCM) != want {
		t.Errorf("decoded PCM = %d bytes, want %d (%d frames)", len(res.AudioPCM), want, res.AudioFrames)
	}
	if len(res.Errors) != 0 {
		t.Errorf("unexpected error frames: %+v", res.Errors)
	}
	if res.Cancelled {
		t.Error("turn was unexpectedly cancelled")
	}
	if !hasDisplay(res.Displays, "", "listening") {
		t.Errorf("never returned to listening; displays=%+v", res.Displays)
	}
}

// TestRunExitPhrase proves the graceful farewell: an exit-phrase turn ends with
// a goodbye frame and a clean close.
func TestRunExitPhrase(t *testing.T) {
	kokoro := constantPCMKokoro(1440)
	defer kokoro.Close()
	llmServer := contentStreamLLM("Goodbye!")
	defer llmServer.Close()

	url, closeSrv := wsURL(t, baseConfig(kokoro.URL, llmServer.URL, "goodbye"))
	defer closeSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := v2client.Run(ctx, v2client.Config{URL: url, MicPCM: micTone(200), Logf: t.Logf})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Goodbye == nil {
		t.Fatal("exit phrase did not produce a goodbye frame")
	}
	if res.Goodbye.Reason != "user_farewell" {
		t.Errorf("goodbye reason = %q, want user_farewell", res.Goodbye.Reason)
	}
	if res.FinalCaption != "Goodbye!" {
		t.Errorf("final caption = %q", res.FinalCaption)
	}
}

// TestRunBargeIn proves the client's abort is honoured: after one TTS frame the
// client sends abort, and the server replies audio_cancel.
func TestRunBargeIn(t *testing.T) {
	kokoro := constantPCMKokoro(2880) // enough frames to interrupt mid-stream
	defer kokoro.Close()
	llmServer := contentStreamLLM("One. ", "Two. ", "Three. ", "Four.")
	defer llmServer.Close()

	url, closeSrv := wsURL(t, baseConfig(kokoro.URL, llmServer.URL, "tell me a story"))
	defer closeSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := v2client.Run(ctx, v2client.Config{
		URL:              url,
		MicPCM:           micTone(300),
		BargeAfterFrames: 1,
		CreditBatch:      1,
		Logf:             t.Logf,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Cancelled {
		t.Error("barge-in did not produce an audio_cancel")
	}
}

// TestRunTools proves the first-class tool path through the real client: the
// server discovers via tool_list, the client answers with a catalog, the LLM
// emits a tool_call, the client returns a result, and the final content speaks.
func TestRunTools(t *testing.T) {
	kokoro := constantPCMKokoro(1440)
	defer kokoro.Close()

	var calls atomic.Int32
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			fmt.Fprintf(w, "data: %s\n\n", s)
			if fl != nil {
				fl.Flush()
			}
		}
		var body struct {
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if n == 1 {
			if len(body.Tools) == 0 {
				t.Error("LLM call 1 saw no tools (discovery did not populate)")
			}
			write(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"self.audio.set_volume","arguments":""}}]}}]}`)
			write(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"volume\":60}"}}]}}]}`)
			write(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
			write("[DONE]")
			return
		}
		write(`{"choices":[{"delta":{"content":"Volume set to 60."}}]}`)
		write(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		write("[DONE]")
	}))
	defer llmServer.Close()

	url, closeSrv := wsURL(t, baseConfig(kokoro.URL, llmServer.URL, "set volume to 60"))
	defer closeSrv()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := v2client.Run(ctx, v2client.Config{
		URL:    url,
		MicPCM: micTone(300),
		Tools: []protov2.ToolDescriptor{{
			Name:        "self.audio.set_volume",
			Description: "Set the audio volume",
			ArgsSchema:  json.RawMessage(`{"type":"object","properties":{"volume":{"type":"integer"}},"required":["volume"]}`),
			Permission:  "public",
		}},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ToolListReqs == 0 {
		t.Error("server never sent a tool_list discovery request")
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(res.ToolCalls))
	}
	if got := res.ToolCalls[0]; got.Name != "self.audio.set_volume" || string(got.Args) != `{"volume":60}` {
		t.Errorf("tool call = name %q args %s", got.Name, string(got.Args))
	}
	if res.FinalCaption != "Volume set to 60." {
		t.Errorf("final caption = %q", res.FinalCaption)
	}
	if calls.Load() != 2 {
		t.Errorf("LLM called %d times, want 2", calls.Load())
	}
}

func hasDisplay(ds []protov2.Display, emotion, status string) bool {
	for _, d := range ds {
		if (emotion == "" || d.Emotion == emotion) && (status == "" || d.Status == status) {
			return true
		}
	}
	return false
}
