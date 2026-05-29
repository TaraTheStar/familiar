// SPDX-License-Identifier: AGPL-3.0-or-later

package session

// Leading wake-beep removal.
//
// When the wake word fires, the firmware plays a short acknowledge chime
// through the speaker. With no AEC that chime bleeds into the microphone and
// lands at the very start of the captured utterance. Whisper transcribes the
// loud transient as "(gasp)"/"*gasp*" and, on a short command, it can drown
// the real speech entirely. The beep is far louder than speech (mean-abs
// ~4000-9000 vs ~200-900), so we detect the initial run of very-loud windows
// and drop up to its end plus a small margin before transcription.

// BeepTrimConfig tunes leading-beep removal. Exported so cmd/stackend can wire
// it to flags for rebuild-free tuning.
type BeepTrimConfig struct {
	// Threshold is the mean-abs amplitude separating the beep from speech.
	// 0 → 2000 (well above speech, well below the chime).
	Threshold float64
	// MaxScanMS bounds how far into the capture we look for the beep, so a
	// loud word later in the sentence can't be mistaken for it. <=0 disables
	// trimming entirely.
	MaxScanMS int
	// MarginMS is dropped in addition to the detected beep tail. 0 → 80.
	MarginMS int
}

func (c BeepTrimConfig) withDefaults() BeepTrimConfig {
	if c.Threshold <= 0 {
		c.Threshold = 2000
	}
	if c.MarginMS <= 0 {
		c.MarginMS = 80
	}
	// MaxScanMS is intentionally not defaulted: 0 means "disabled" so callers
	// (and tests) opt in explicitly. cmd/stackend's flag default turns it on.
	return c
}

// stripLeadingBeep returns the PCM with the leading wake-beep removed and how
// many milliseconds were dropped (0 if none found or trimming disabled).
func stripLeadingBeep(pcm []int16, sampleRate int, cfg BeepTrimConfig) ([]int16, int) {
	cfg = cfg.withDefaults()
	if cfg.MaxScanMS <= 0 || sampleRate <= 0 {
		return pcm, 0
	}
	const winMS = 20
	win := sampleRate * winMS / 1000
	if win == 0 {
		return pcm, 0
	}
	maxWins := cfg.MaxScanMS / winMS

	// Find the last 20ms window within the scan region whose energy is at
	// beep level. Scanning to the last (rather than stopping at the first
	// quiet gap) absorbs brief dips inside the chime.
	lastLoud := -1
	for i := 0; i < maxWins; i++ {
		start := i * win
		if start+win > len(pcm) {
			break
		}
		if meanAbs(pcm[start:start+win]) >= cfg.Threshold {
			lastLoud = i
		}
	}
	if lastLoud < 0 {
		return pcm, 0 // no beep detected
	}

	cut := (lastLoud+1)*win + sampleRate*cfg.MarginMS/1000
	if cut >= len(pcm) {
		return pcm, 0 // would remove everything — bail, let ASR try the raw audio
	}
	return pcm[cut:], cut * 1000 / sampleRate
}
