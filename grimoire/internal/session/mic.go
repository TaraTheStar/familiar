// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "context"

// fireTurn ends the current listening window and dispatches the captured
// utterance into the pipeline. Called from two places, both on the read-loop
// goroutine: the server-side endpoint detector (auto-stop mode) and an
// incoming device listen:stop (manual mode). The `reason` is logged so we can
// tell which path fired when reading logs.
//
// It must run on the read-loop goroutine (no locking on listening/micBuf/ep)
// and it hands the actual ASR→LLM→TTS work to a goroutine so the read loop
// keeps pumping frames (and MCP responses) while the turn runs.
func (s *Session) fireTurn(reason string) {
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
		go func() {
			if err := s.Speak(context.Background(), s.cfg.HardcodedReply); err != nil {
				s.log.Warn("hardcoded reply failed", "err", err)
			}
		}()
	case s.cfg.ASR != nil && s.cfg.LLM != nil:
		s.log.Info("turn fired", "reason", reason, "pcm_bytes", len(turn), "approx_ms", approxMS)
		go s.handleTurn(context.Background(), turn)
	case len(turn) > 0:
		s.log.Info("captured utterance (no pipeline configured)",
			"reason", reason, "pcm_bytes", len(turn), "approx_ms", approxMS)
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
