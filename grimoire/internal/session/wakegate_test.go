// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "testing"

func TestWakeGateRejects(t *testing.T) {
	cases := []struct {
		transcript string
		reject     bool
	}{
		// Real false-wake transcripts observed on hardware, 2026-07-16.
		{"Hey lunch.  That's all.", true},
		{"Hey, lunch. That's all.", true},
		{"Hey lunch. That's all.", true},
		{"Hey, launch. That's all.", true},
		{"Hey lunch.", true},
		{"Lunchtime. That's all.", true},

		// Real genuine-wake transcripts — must pass, including whisper's
		// fuzzy renderings of the wake word.
		{"Hey Luna. That's all.", false},
		{"Hey Luna, that's all.", false},
		{"Hey, Mila.  That's all.", false},
		{"Hey Luna.   Tell me a funny story.", false},
		{"That's all.", false}, // wake phrase didn't leak into the turn

		// Adversarial head but substantive content — fail open to the LLM.
		{"Hey lunch is ready, come downstairs!", false},
		{"Lunch ideas for tomorrow please", false},

		// Edge cases.
		{"", false},
		{"...", false},
		{"Hey.", false},
	}
	for _, c := range cases {
		if got := wakeGateRejects(c.transcript); got != c.reject {
			t.Errorf("wakeGateRejects(%q) = %v, want %v", c.transcript, got, c.reject)
		}
	}
}
