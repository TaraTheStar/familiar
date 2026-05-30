// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "context"

// fireTurn ends the current listening window and dispatches the captured
// utterance into the pipeline. Called from three places, all on the read-loop
// goroutine: the server-side endpoint detector (auto-stop mode), an incoming
// device listen_stop (manual mode), and a buffer-full force-endpoint. The
// `reason` is logged so we can tell which path fired when reading logs.
//
// It must run on the read-loop goroutine (no locking on listening/micBuf/ep)
// and it hands the actual ASR→LLM→TTS work to a goroutine so the read loop
// keeps pumping frames (and tool responses) while the turn runs.
//
// The turn runs under a cancelable child of the session context, its cancel
// func stored on the session so a barge-in (abort) can interrupt the in-flight
// ASR/LLM/TTS (see cancelTurn). ctx is the session context.
func (s *Session) fireTurn(ctx context.Context, reason string) {
	if !s.listening {
		return // already fired this turn; ignore duplicate triggers
	}
	s.listening = false
	s.ep = nil
	turn := s.takeMicTurn()
	approxMS := 0
	if s.cfg.MicAudio.SampleRate > 0 {
		approxMS = len(turn) / 2 * 1000 / s.cfg.MicAudio.SampleRate
	}

	switch {
	case s.cfg.HardcodedReply != "":
		s.log.Info("turn fired (hardcoded reply)", "reason", reason, "pcm_bytes", len(turn))
		turnCtx, cancel := s.beginTurn(ctx)
		go func() {
			defer cancel()
			if err := s.speakReply(turnCtx, s.cfg.HardcodedReply); err != nil {
				s.log.Warn("hardcoded reply failed", "err", err)
			}
		}()
	case s.cfg.ASR != nil && s.cfg.LLM != nil:
		s.log.Info("turn fired", "reason", reason, "pcm_bytes", len(turn), "approx_ms", approxMS)
		turnCtx, cancel := s.beginTurn(ctx)
		go func() {
			defer cancel()
			s.handleTurn(turnCtx, turn)
		}()
	case len(turn) > 0:
		s.log.Info("captured utterance (no pipeline configured)",
			"reason", reason, "pcm_bytes", len(turn), "approx_ms", approxMS)
	}
}

// beginTurn derives a cancelable context for a turn and records its cancel func
// so cancelTurn (barge-in) can interrupt it. Read-loop goroutine only, so no
// locking: fireTurn and cancelTurn never run concurrently. The turn goroutine
// must defer the returned cancel to release the context when it ends.
func (s *Session) beginTurn(ctx context.Context) (context.Context, context.CancelFunc) {
	turnCtx, cancel := context.WithCancel(ctx)
	s.turnCancel = cancel
	return turnCtx, cancel
}

// cancelTurn interrupts the in-flight turn, if any. Idempotent: a finished
// turn's cancel is a harmless no-op. Read-loop goroutine only.
func (s *Session) cancelTurn() {
	if s.turnCancel != nil {
		s.turnCancel()
	}
}

// takeMicTurn hands ownership of the current mic buffer to the caller and
// resets the session's buffer to an empty slice (same backing array, so
// memory stays warm for the next turn).
//
// Callers (whisper, telemetry) get a stable []byte they can hold across
// the next turn without races. The session keeps its growth-capped buffer
// for reuse on the next listen:start.
func (s *Session) takeMicTurn() []byte {
	out := append([]byte(nil), s.micBuf...)
	s.micBuf = s.micBuf[:0]
	return out
}
