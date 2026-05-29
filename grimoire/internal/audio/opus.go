// SPDX-License-Identifier: AGPL-3.0-or-later

// Package audio handles PCM ↔ Opus conversion and frame-level helpers for
// the voice loop. cgo against libopus; the runtime container has to bundle
// libopus0.
package audio

import (
	"errors"
	"fmt"

	"gopkg.in/hraban/opus.v2"
)

// Encoder is a one-direction Opus encoder, configured at construction time
// for a specific (sample_rate, channels, frame_ms) tuple. Not safe for
// concurrent use — give each goroutine its own.
type Encoder struct {
	enc             *opus.Encoder
	sampleRate      int
	channels        int
	frameMS         int
	samplesPerFrame int
	scratch         []byte
}

// NewEncoder constructs an encoder for the given audio format. Typical
// StackChan use is (24000, 1, 60) — matching the server hello we advertise
// to the device.
//
// libopus only supports sample rates 8000/12000/16000/24000/48000 and
// frame durations 2.5/5/10/20/40/60 ms. Anything else will return an error.
func NewEncoder(sampleRate, channels, frameMS int) (*Encoder, error) {
	if channels < 1 || channels > 2 {
		return nil, fmt.Errorf("audio: channels must be 1 or 2, got %d", channels)
	}
	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("audio: NewEncoder: %w", err)
	}

	samples := sampleRate * frameMS / 1000
	if samples*1000 != sampleRate*frameMS {
		return nil, fmt.Errorf("audio: frame_ms=%d does not divide evenly into sample_rate=%d", frameMS, sampleRate)
	}

	return &Encoder{
		enc:             enc,
		sampleRate:      sampleRate,
		channels:        channels,
		frameMS:         frameMS,
		samplesPerFrame: samples,
		// 4000 bytes is libopus's recommended max packet size; voice
		// frames at 24kHz/60ms typically come in well under 300 bytes.
		scratch: make([]byte, 4000),
	}, nil
}

// SamplesPerFrame returns the number of int16 samples (per channel) that
// Encode expects in each call. For 24000Hz/60ms = 1440.
func (e *Encoder) SamplesPerFrame() int { return e.samplesPerFrame }

// SampleRate returns the configured sample rate.
func (e *Encoder) SampleRate() int { return e.sampleRate }

// FrameMS returns the configured frame duration.
func (e *Encoder) FrameMS() int { return e.frameMS }

// ErrShortFrame is returned by Encode when the input PCM doesn't contain
// exactly SamplesPerFrame * channels samples.
var ErrShortFrame = errors.New("audio: input must be exactly one frame")

// Encode compresses one frame of PCM (signed 16-bit, interleaved if stereo)
// into an Opus packet. The returned slice is owned by the encoder and is
// reused on the next call — copy if you need to keep it.
func (e *Encoder) Encode(pcm []int16) ([]byte, error) {
	if len(pcm) != e.samplesPerFrame*e.channels {
		return nil, ErrShortFrame
	}
	n, err := e.enc.Encode(pcm, e.scratch)
	if err != nil {
		return nil, fmt.Errorf("audio: encode: %w", err)
	}
	return e.scratch[:n], nil
}

// FrameBytes returns the number of PCM bytes per frame (samples * 2 bytes
// per int16 * channels). Useful when slicing raw PCM streams.
func (e *Encoder) FrameBytes() int {
	return e.samplesPerFrame * e.channels * 2
}
