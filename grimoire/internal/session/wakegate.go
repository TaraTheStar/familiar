// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"regexp"
	"strings"
)

// Wake gate — a server-side backstop for adversarial false wakes.
//
// The on-device microWakeWord model confuses "Hey Loona" with a small family
// of near-phrases ("hey lunch" being the worst measured offender: it scored
// inside the genuine-wake probability band on real hardware, 2026-07-16).
// When such a false wake opens a session, the wake phrase itself leaks into
// the first turn's pre-roll audio and whisper transcribes it faithfully —
// so the transcript is a second, much more discriminating classifier.
//
// The gate is deliberately narrow, and fails open:
//   - only the FIRST turn after a wake-word pre-roll is examined;
//   - only transcripts whose head is a known adversarial word are candidates;
//   - anything Loona-like anywhere in the transcript passes (whisper renders
//     genuine wakes as "Luna", "Loona", occasionally "Mila" — fuzzy renderings
//     share the vowel-N core, unlike "lunch");
//   - substantive content after the head passes too, so a real "hey lunch is
//     ready, tell me a joke" style turn still reaches the LLM.
//
// A rejected turn closes the session silently (no LLM, no TTS) and the
// device returns to idle, exactly as if the wake had never fired.

// wakeGateAdversarialHead is the family of near-phrases the on-device model
// is known to confuse with the wake word. Head tokens only — keep in sync
// with the adversarial negatives fed to training.
var wakeGateAdversarialHead = map[string]bool{
	"lunch":     true,
	"launch":    true,
	"brunch":    true,
	"munch":     true,
	"crunch":    true,
	"lunge":     true,
	"lunchtime": true,
}

// wakeGateWakeLike matches transcript tokens that plausibly ARE the wake
// word as whisper renders it. Generous on purpose: rejecting a genuine wake
// costs a whole interaction, passing a false one costs a beep.
var wakeGateWakeLike = regexp.MustCompile(`^(loona|luna|louna|loonah|lunah?|mila|luena)$`)

var wakeGateTokenizer = regexp.MustCompile(`[a-z']+`)

// wakeGateRejects reports whether the first-turn transcript after a
// wake-word pre-roll looks like a bare adversarial false wake.
func wakeGateRejects(transcript string) bool {
	tokens := wakeGateTokenizer.FindAllString(strings.ToLower(transcript), -1)
	if len(tokens) == 0 {
		return false
	}

	// Anything wake-word-like anywhere → genuine wake, pass.
	for _, t := range tokens {
		if wakeGateWakeLike.MatchString(t) {
			return false
		}
	}

	// Skip a leading "hey" ("hay" is a common whisper rendering).
	i := 0
	if tokens[0] == "hey" || tokens[0] == "hay" {
		i++
	}
	if i >= len(tokens) || !wakeGateAdversarialHead[tokens[i]] {
		return false
	}

	// Adversarial head. Reject only if the rest is empty or a bare exit
	// phrase — the signature of a false wake (nobody was addressing the
	// device). Real content after the head fails open to the LLM.
	rest := strings.Join(tokens[i+1:], " ")
	rest = exitPhrasePattern.ReplaceAllString(rest, "")
	return len(wakeGateTokenizer.FindAllString(rest, -1)) < 2
}
