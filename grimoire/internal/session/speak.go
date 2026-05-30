// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/TaraTheStar/familiar/grimoire/internal/audio"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// speakSentence synthesizes one sentence via Kokoro and streams it into the
// already-open reply (s.out must be between SpeakBegin and SpeakEnd). It sends
// the caption then the audio so the chat bubble updates as each sentence
// plays. Kokoro fetch is wire-agnostic and stays in the loop; the protocol
// implementation (s.out) owns captioning and audio flow control.
func (s *Session) speakSentence(ctx context.Context, text string) error {
	if s.cfg.Kokoro == nil {
		return fmt.Errorf("session: Kokoro client not configured")
	}
	log := s.log.With("text", truncate(text, 60))
	log.Info("speak segment begin")

	// Body streams as Kokoro generates; bound the fetch so a stuck TTS server
	// can't wedge the turn.
	kokoroCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	pcm, err := s.cfg.Kokoro.PCM(kokoroCtx, text)
	if err != nil {
		return fmt.Errorf("kokoro: %w", err)
	}
	defer pcm.Close()

	// Kokoro only emits 24kHz. When we dictate a lower TTS rate to the device
	// (so it plays directly with no on-device resample), downsample here on the
	// server — far cheaper than on the ESP32's audio core, where it was
	// competing with wakenet + AEC during playback and starving wake-word
	// detection (which broke barge-in).
	var src io.Reader = pcm
	if s.cfg.TTSAudio.SampleRate != tts.KokoroSampleRate {
		src = audio.NewResampleReader(pcm, tts.KokoroSampleRate, s.cfg.TTSAudio.SampleRate)
	}

	if err := s.out.Caption(ctx, text); err != nil {
		return fmt.Errorf("caption: %w", err)
	}
	if err := s.out.SpeakPCM(ctx, src); err != nil {
		return fmt.Errorf("speak pcm: %w", err)
	}
	log.Info("speak segment end")
	return nil
}

// speakReply speaks a complete, standalone reply (one SpeakBegin … SpeakEnd
// pair) from a fixed set of sentences. Convenience for one-shot callers (the
// hardcoded-reply debug path). The streaming voice loop in runToolLoop drives
// s.out directly so each sentence is spoken as it falls out of the LLM stream
// while staying inside a single reply.
func (s *Session) speakReply(ctx context.Context, sentences ...string) error {
	if len(sentences) == 0 {
		return nil
	}
	// Smile while speaking (matches the v1 begin-of-reply emotion).
	_ = s.out.Display(ctx, "happy", "speaking")
	if err := s.out.SpeakBegin(ctx); err != nil {
		return err
	}
	defer s.out.SpeakEnd(ctx)
	for _, sentence := range sentences {
		if err := s.speakSentence(ctx, sentence); err != nil {
			return err
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
