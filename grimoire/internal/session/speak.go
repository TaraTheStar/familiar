// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/audio"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
)

// ttsLead is how far ahead of real time we keep the device's playback queue
// while streaming TTS. Big enough to absorb encode/network jitter (and the
// Kokoro fetch gap between sentences within a turn) without an audible
// underrun; small enough that tts:stop lands close to true playback end so
// the mic doesn't re-open while the speaker is still talking.
const ttsLead = 400 * time.Millisecond

// ttsSession streams one logical reply to the device as a SINGLE Speaking
// session — one tts:start … tts:stop pair — even when the reply is several
// sentences. Each sentence is sent as its own sentence_start caption plus
// frames, but the device never leaves Speaking between them.
//
// Why one session per reply: if we sent tts:stop after each sentence the
// device would flip Speaking→Listening (re-opening the mic) and the next
// sentence's tts:start would race that transition — truncating the second
// (and later) sentences. A joke's punchline ("…to get to the other side!")
// is exactly the second-sentence case that kept getting cut. Holding one
// session open makes consecutive sentences play gaplessly.
//
// The frame clock (start + frameCount) is continuous across sentences, so
// the lead buffer absorbs the brief Kokoro fetch gap between them; if a
// fetch runs longer than the lead we simply fall behind and burst to catch
// up (a short audible pause, never a cut).
//
// Not safe for concurrent use: a session holds s.speakMu for its whole
// lifetime so two replies can't interleave on the wire.
type ttsSession struct {
	s          *Session
	started    bool // tts:start sent + speakMu held
	frameCount int
	start      time.Time
	frameDur   time.Duration
}

// newTTSSession creates a reply-scoped speaker. Nothing is sent and no lock
// is taken until the first Speak — a tool-only turn that never speaks costs
// nothing and need not (but may) Close.
func (s *Session) newTTSSession() *ttsSession {
	return &ttsSession{
		s:        s,
		frameDur: time.Duration(s.encoder.FrameMS()) * time.Millisecond,
	}
}

// begin lazily opens the device Speaking session on the first sentence:
// grabs speakMu, drives the avatar to "happy", and sends tts:start.
func (t *ttsSession) begin(ctx context.Context) error {
	s := t.s
	if s.cfg.Kokoro == nil {
		return errors.New("session: Kokoro client not configured")
	}
	if s.encoder == nil {
		return errors.New("session: opus encoder not configured")
	}
	s.speakMu.Lock()
	s.setEmotion(ctx, "happy")
	if err := writeJSON(ctx, s.conn, protocol.TTS{Type: "tts", State: "start"}); err != nil {
		s.speakMu.Unlock()
		return fmt.Errorf("send tts:start: %w", err)
	}
	t.started = true
	t.start = time.Now()
	return nil
}

// Speak synthesizes one sentence and streams it into the open session.
// Opens the session on first call. On a send/encode error it tears the
// session down (tts:stop) so the device leaves Speaking.
func (t *ttsSession) Speak(ctx context.Context, text string) error {
	s := t.s
	if !t.started {
		if err := t.begin(ctx); err != nil {
			return err
		}
	}

	log := s.log.With("text", truncate(text, 60))
	log.Info("speak segment begin")

	// Fetch PCM from Kokoro. Body streams as Kokoro generates.
	kokoroCtx, cancelKokoro := context.WithTimeout(ctx, 60*time.Second)
	defer cancelKokoro()
	pcmBody, err := s.cfg.Kokoro.PCM(kokoroCtx, text)
	if err != nil {
		return fmt.Errorf("kokoro: %w", err)
	}
	defer pcmBody.Close()

	// Per-sentence caption so the device's chat bubble updates as each
	// sentence is spoken. Still inside the one Speaking session.
	if err := writeJSON(ctx, s.conn, protocol.TTS{
		Type: "tts", State: "sentence_start", Text: text,
	}); err != nil {
		return fmt.Errorf("send sentence_start: %w", err)
	}

	// Stream PCM through the encoder, one binary WS frame per Opus packet,
	// paced at ~real time against the session-wide clock (see type doc).
	framer := audio.NewPCMFramer(pcmBody, s.encoder.SamplesPerFrame())
	for framer.Next() {
		opusBytes, err := s.encoder.Encode(framer.Frame())
		if err != nil {
			return fmt.Errorf("opus encode frame %d: %w", t.frameCount, err)
		}
		// Copy because the encoder reuses its scratch slice.
		out := append([]byte(nil), opusBytes...)
		if err := s.conn.Write(ctx, websocket.MessageBinary, out); err != nil {
			return fmt.Errorf("send opus frame %d: %w", t.frameCount, err)
		}
		t.frameCount++

		// Sleep until this frame's real-time slot (minus the lead buffer).
		// Negative durations (the initial prime burst, or catching up after
		// a slow fetch) don't sleep.
		target := t.start.Add(time.Duration(t.frameCount)*t.frameDur - ttsLead)
		if d := time.Until(target); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := framer.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("pcm read: %w", err)
	}

	log.Info("speak segment end", "frames", t.frameCount)
	return nil
}

// Close ends the Speaking session: drains the lead buffer so tts:stop lands
// at true end-of-audio, sends tts:stop, and releases speakMu. No-op if the
// session was never opened. Safe to call (and defer) unconditionally.
func (t *ttsSession) Close(ctx context.Context) error {
	if !t.started {
		return nil
	}
	t.started = false
	s := t.s
	defer s.speakMu.Unlock()

	// We've been running ttsLead ahead of real playback. Wait out that lead
	// (against the same clock) so tts:stop arrives as the audio truly ends,
	// not ~400ms early — otherwise the device flips to Listening while the
	// tail of the last word is still playing and clips it.
	target := t.start.Add(time.Duration(t.frameCount) * t.frameDur)
	if d := time.Until(target); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			_ = writeJSON(context.WithoutCancel(ctx), s.conn, protocol.TTS{Type: "tts", State: "stop"})
			return ctx.Err()
		}
	}

	if err := writeJSON(ctx, s.conn, protocol.TTS{Type: "tts", State: "stop"}); err != nil {
		return fmt.Errorf("send tts:stop: %w", err)
	}
	s.log.Info("speak end", "frames", t.frameCount, "audio_ms", t.frameCount*s.encoder.FrameMS())
	return nil
}

// Speak synthesizes `text` and streams it to the device as a complete,
// standalone Speaking session (tts:start … tts:stop). Convenience for
// one-shot callers (e.g. the hardcoded-reply debug path). The voice loop
// uses newTTSSession directly so a multi-sentence reply stays one session.
func (s *Session) Speak(ctx context.Context, text string) error {
	t := s.newTTSSession()
	if err := t.Speak(ctx, text); err != nil {
		_ = t.Close(ctx)
		return err
	}
	return t.Close(ctx)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
