// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// recordingOut is a deviceOut that records the loop's seam calls instead of
// putting anything on a wire. It is the protocol-agnostic safety net: it pins
// the ORDER and ARGUMENTS the voice loop drives the seam with, independent of
// v1/v2 framing. If a future protocol (or a loop refactor) changes that
// contract, this test catches it without depending on byte-level wire output —
// the v1/v2 golden tests cover the framing, this covers the choreography.
type recordingOut struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingOut) record(s string) {
	r.mu.Lock()
	r.calls = append(r.calls, s)
	r.mu.Unlock()
}

func (r *recordingOut) Transcript(_ context.Context, text string) error {
	r.record("Transcript " + text)
	return nil
}

func (r *recordingOut) Display(_ context.Context, emotion, status string) error {
	r.record("Display " + emotion + "/" + status)
	return nil
}

func (r *recordingOut) SpeakBegin(_ context.Context) error {
	r.record("SpeakBegin")
	return nil
}

func (r *recordingOut) Caption(_ context.Context, segment string) error {
	r.record("Caption " + segment)
	return nil
}

func (r *recordingOut) SpeakPCM(_ context.Context, pcm io.Reader) error {
	_, _ = io.Copy(io.Discard, pcm) // drain so Kokoro's body closes cleanly
	r.record("SpeakPCM")
	return nil
}

func (r *recordingOut) SpeakEnd(_ context.Context) error {
	r.record("SpeakEnd")
	return nil
}

func (r *recordingOut) Close(_ context.Context, reason string) error {
	r.record("Close " + reason)
	return nil
}

func (r *recordingOut) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// newLoopTestSession builds a Session wired to real httptest-backed Kokoro/LLM
// and a fake ASR, but with a recordingOut in place of any wire implementation,
// so handleTurn can be driven directly (no WebSocket, no protocol). The caller
// closes the returned servers.
func newLoopTestSession(t *testing.T, transcript string, deltas ...string) (*Session, *recordingOut, func()) {
	t.Helper()
	kokoro := constantPCMKokoro(t, 1440) // 60ms @24kHz = exactly 1 frame/sentence
	llmServer := sentenceStreamLLM(t, deltas...)
	out := &recordingOut{}
	s := &Session{
		cfg: Config{
			TTSAudio: protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
			MicAudio: protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
			Kokoro:   &tts.KokoroClient{BaseURL: kokoro.URL},
			ASR:      &fakeASR{out: transcript},
			LLM:      &llm.Client{BaseURL: llmServer.URL, Model: "test-model"},
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		out: out,
	}
	cleanup := func() {
		kokoro.Close()
		llmServer.Close()
	}
	return s, out, cleanup
}

// TestSeamContractMultiSentence pins the exact sequence and arguments the loop
// drives the seam with for a normal multi-sentence reply. This is the contract
// every deviceOut implementation must satisfy; v1Out and v2Out are just two
// renderings of it.
func TestSeamContractMultiSentence(t *testing.T) {
	s, out, cleanup := newLoopTestSession(t, "what time is it", "Hello there. ", "How are you", "?")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.handleTurn(ctx, make([]byte, 3200)) // 100ms of 16kHz s16; content irrelevant (ASR faked)

	want := []string{
		"Transcript what time is it",
		"Display thinking/thinking",
		"Display happy/speaking", // lazy begin: smile fires on the first voiced sentence
		"SpeakBegin",
		"Caption Hello there.",
		"SpeakPCM",
		"Caption How are you?",
		"SpeakPCM",
		"SpeakEnd",
		"Display neutral/listening",
	}
	assertCalls(t, want, out.snapshot())
}

// TestSeamContractExitPhrase pins that an exit-phrase turn ends with Close after
// the reply and the neutral reset — the protocol-agnostic farewell contract.
func TestSeamContractExitPhrase(t *testing.T) {
	s, out, cleanup := newLoopTestSession(t, "goodbye", "Goodbye!")
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.handleTurn(ctx, make([]byte, 3200))

	want := []string{
		"Transcript goodbye",
		"Display thinking/thinking",
		"Display happy/speaking",
		"SpeakBegin",
		"Caption Goodbye!",
		"SpeakPCM",
		"SpeakEnd",
		"Display neutral/listening",
		"Close goodbye",
	}
	assertCalls(t, want, out.snapshot())
}

func assertCalls(t *testing.T, want, got []string) {
	t.Helper()
	if len(got) != len(want) || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("seam call sequence mismatch.\n--- got ---\n%s\n--- want ---\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
