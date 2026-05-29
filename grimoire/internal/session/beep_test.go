// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "testing"

// tone builds n samples of constant mean-abs amplitude (alternating sign).
func tone(amp int16, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		if i%2 == 0 {
			out[i] = amp
		} else {
			out[i] = -amp
		}
	}
	return out
}

func TestStripLeadingBeep(t *testing.T) {
	const sr = 16000
	// 300ms loud beep (~6000) + 1000ms speech (~400).
	beep := tone(6000, sr*300/1000)
	speech := tone(400, sr*1000/1000)
	pcm := append(append([]int16{}, beep...), speech...)

	trimmed, dropped := stripLeadingBeep(pcm, sr, BeepTrimConfig{MaxScanMS: 600})
	if dropped == 0 {
		t.Fatal("expected the beep to be trimmed")
	}
	// Should drop roughly the beep length (300ms) plus the 80ms margin.
	if dropped < 300 || dropped > 460 {
		t.Errorf("dropped %dms, want ~380ms", dropped)
	}
	// What remains should be (quiet) speech only — no beep-level energy.
	if meanAbs(trimmed) > 1000 {
		t.Errorf("trimmed audio still loud (mean-abs %.0f) — beep not removed", meanAbs(trimmed))
	}
}

func TestStripLeadingBeepNoBeep(t *testing.T) {
	const sr = 16000
	pcm := tone(400, sr) // 1s of plain speech, no beep
	trimmed, dropped := stripLeadingBeep(pcm, sr, BeepTrimConfig{MaxScanMS: 600})
	if dropped != 0 || len(trimmed) != len(pcm) {
		t.Errorf("trimmed clean speech: dropped=%d len %d->%d", dropped, len(pcm), len(trimmed))
	}
}

func TestStripLeadingBeepDisabled(t *testing.T) {
	const sr = 16000
	pcm := append(tone(6000, sr*300/1000), tone(400, sr)...)
	trimmed, dropped := stripLeadingBeep(pcm, sr, BeepTrimConfig{MaxScanMS: 0})
	if dropped != 0 || len(trimmed) != len(pcm) {
		t.Errorf("MaxScanMS=0 should disable trimming: dropped=%d", dropped)
	}
}
