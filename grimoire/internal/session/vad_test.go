// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "testing"

// frame returns one 60ms-equivalent frame of constant-amplitude PCM.
func frame(amp int16, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		// Alternate sign so meanAbs == amp regardless of DC.
		if i%2 == 0 {
			out[i] = amp
		} else {
			out[i] = -amp
		}
	}
	return out
}

func TestEndpointerFiresAfterSpeechThenSilence(t *testing.T) {
	ep := newEndpointer(EndpointConfig{FrameMS: 60, MinSpeechMS: 120, EndSilenceMS: 300, MinThreshold: 350})

	// 5 loud frames (300ms speech) — should not fire yet.
	for i := 0; i < 5; i++ {
		if ep.update(frame(1000, 960)) {
			t.Fatalf("fired during speech at frame %d", i)
		}
	}
	// Silence frames: needs 300ms (5 frames) of trailing silence.
	fired := false
	for i := 0; i < 5; i++ {
		if ep.update(frame(10, 960)) {
			fired = true
			break
		}
	}
	if !fired {
		t.Fatal("never endpointed after speech + 300ms silence")
	}
	// After firing, further updates must return false (latched).
	if ep.update(frame(10, 960)) {
		t.Fatal("fired twice")
	}
}

func TestEndpointerIgnoresLeadingSilence(t *testing.T) {
	ep := newEndpointer(EndpointConfig{FrameMS: 60, MinSpeechMS: 120, EndSilenceMS: 180, MinThreshold: 350})
	// Long leading silence must never endpoint (no speech heard yet).
	for i := 0; i < 50; i++ {
		if ep.update(frame(5, 960)) {
			t.Fatalf("fired on leading silence at frame %d", i)
		}
	}
}

func TestEndpointerIgnoresBeep(t *testing.T) {
	ep := newEndpointer(EndpointConfig{FrameMS: 60, MinSpeechMS: 120, EndSilenceMS: 300, MinThreshold: 180, BeepCeiling: 2500})

	// The wake beep: very loud frames. Must NOT count as speech and must NOT
	// arm the endpoint, even followed by silence.
	for i := 0; i < 4; i++ {
		ep.update(frame(6000, 960))
	}
	for i := 0; i < 10; i++ {
		if ep.update(frame(60, 960)) {
			t.Fatalf("endpointed on beep+silence (no real speech) at frame %d", i)
		}
	}
	// Now real speech arrives, then trailing silence — should endpoint.
	for i := 0; i < 3; i++ {
		ep.update(frame(900, 960))
	}
	fired := false
	for i := 0; i < 6; i++ {
		if ep.update(frame(60, 960)) {
			fired = true
			break
		}
	}
	if !fired {
		t.Fatal("never endpointed after the beep was ignored and real speech followed")
	}
}

func TestEndpointerRequiresMinSpeech(t *testing.T) {
	ep := newEndpointer(EndpointConfig{FrameMS: 60, MinSpeechMS: 300, EndSilenceMS: 120, MinThreshold: 350})
	// Only 120ms of speech (< 300ms min) then silence — must not fire.
	ep.update(frame(1000, 960))
	ep.update(frame(1000, 960))
	for i := 0; i < 10; i++ {
		if ep.update(frame(10, 960)) {
			t.Fatalf("fired with only 120ms speech at silence frame %d", i)
		}
	}
}
