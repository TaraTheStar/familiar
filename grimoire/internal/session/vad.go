// SPDX-License-Identifier: AGPL-3.0-or-later

package session

// Server-side voice endpointing.
//
// Why this exists: in auto-stop mode (the device's default when AEC is off)
// the firmware's audio front-end streams the microphone CONTINUOUSLY for the
// whole listening window. Its on-device VAD only drives the status LED — it
// never sends a listen:stop. So the *server* is responsible for deciding when
// the user has finished speaking and the turn should fire. (Manual-stop mode
// is different: there the device sends listen:stop on button release, and we
// don't run this detector.)
//
// This is a pragmatic energy-based endpointer: it tracks an adaptive noise
// floor and flags speech when a frame's mean-absolute amplitude rises well
// above that floor. Once it has heard enough speech, a sufficient run of
// trailing silence ends the utterance. It runs entirely on the read-loop
// goroutine (one Update per decoded mic frame), so it needs no locking.
//
// It can be swapped for a Silero model later behind the same Update() shape;
// the energy detector is good enough to get the loop closing and gives us
// real per-frame numbers in the logs to tune against.

// EndpointConfig holds the tunables. Zero values are filled with defaults
// by newEndpointer.
type EndpointConfig struct {
	// FrameMS is the duration of one mic frame (matches MicAudio.FrameDuration).
	FrameMS int
	// MinSpeechMS requires this much cumulative speech before any amount of
	// silence is allowed to end the turn. Guards against a single cough or
	// the wake-word tail endpointing an empty utterance.
	MinSpeechMS int
	// EndSilenceMS is the trailing-silence run (after speech) that ends the
	// utterance. Too low → we cut the user off mid-pause; too high → laggy.
	EndSilenceMS int
	// SpeechFactor multiplies the adaptive noise floor to get the speech
	// threshold. Higher → less sensitive.
	SpeechFactor float64
	// MinThreshold is an absolute floor on the speech threshold so a dead-
	// silent room (noise floor ≈ 0) doesn't make every tiny sample "speech".
	MinThreshold float64
	// BeepCeiling: frames louder than this are ignored — neither speech nor
	// silence, and they don't move the noise floor. This is the wake-
	// acknowledge beep (mean-abs ~4000-9000), which is far above speech
	// (~200-900) and would otherwise count as speech and arm the endpoint
	// before the user has said anything. 0 → 2500.
	BeepCeiling float64
}

func (c EndpointConfig) withDefaults() EndpointConfig {
	if c.FrameMS <= 0 {
		c.FrameMS = 60
	}
	if c.MinSpeechMS <= 0 {
		c.MinSpeechMS = 240
	}
	if c.EndSilenceMS <= 0 {
		c.EndSilenceMS = 800
	}
	if c.SpeechFactor <= 0 {
		c.SpeechFactor = 2.5
	}
	if c.MinThreshold <= 0 {
		c.MinThreshold = 180
	}
	if c.BeepCeiling <= 0 {
		c.BeepCeiling = 2500
	}
	return c
}

// endpointer is the per-turn detector state. Constructed fresh on each
// listen:start so noise-floor adaptation restarts cleanly.
type endpointer struct {
	cfg EndpointConfig

	// floor is the adaptive noise floor (mean-abs amplitude). It starts at 0
	// and rises slowly, so a loud first frame (the user speaking immediately)
	// can't pin the floor high and desensitize the detector. MinThreshold is
	// the real gate until the room's actual noise floor climbs above it.
	floor float64

	speechMS  int  // cumulative speech heard this turn
	silenceMS int  // current trailing-silence run
	ended     bool // latched true once we endpoint, so we fire exactly once

	// Last-frame diagnostics, exposed for per-frame debug logging / tuning.
	lastEnergy    float64
	lastThreshold float64
}

func newEndpointer(cfg EndpointConfig) *endpointer {
	return &endpointer{cfg: cfg.withDefaults()}
}

// update feeds one frame of PCM and reports whether the utterance has just
// ended. Returns false once it has already ended (so repeated calls are safe).
func (e *endpointer) update(pcm []int16) bool {
	if e.ended || len(pcm) == 0 {
		return false
	}
	energy := meanAbs(pcm)
	e.lastEnergy = energy

	// Ignore the wake-acknowledge beep and other loud transients: they're far
	// louder than speech and would otherwise count as speech (arming the
	// endpoint so the pause before the user talks ends the turn early) and
	// drag the noise floor up (desensitizing detection of the real, quieter
	// command). Skip entirely — neither speech, silence, nor a floor update.
	if energy > e.cfg.BeepCeiling {
		e.lastThreshold = e.cfg.BeepCeiling
		return false
	}

	// Adapt the noise floor: drop fast toward quiet frames, rise slowly so a
	// burst of speech doesn't drag the floor up and desensitize us.
	if energy < e.floor {
		e.floor = e.floor*0.85 + energy*0.15
	} else {
		e.floor = e.floor*0.995 + energy*0.005
	}

	threshold := e.floor * e.cfg.SpeechFactor
	if threshold < e.cfg.MinThreshold {
		threshold = e.cfg.MinThreshold
	}
	e.lastThreshold = threshold

	if energy > threshold {
		e.speechMS += e.cfg.FrameMS
		e.silenceMS = 0
		return false
	}

	// Below threshold. Only count silence once we've actually heard speech;
	// leading silence (and the gap before the user starts) shouldn't end the
	// turn.
	if e.speechMS == 0 {
		return false
	}
	e.silenceMS += e.cfg.FrameMS
	if e.speechMS >= e.cfg.MinSpeechMS && e.silenceMS >= e.cfg.EndSilenceMS {
		e.ended = true
		return true
	}
	return false
}

// meanAbs is the mean absolute amplitude of a frame — a cheap stand-in for
// RMS that behaves the same for endpointing purposes.
func meanAbs(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum int64
	for _, s := range pcm {
		if s < 0 {
			sum -= int64(s)
		} else {
			sum += int64(s)
		}
	}
	return float64(sum) / float64(len(pcm))
}
