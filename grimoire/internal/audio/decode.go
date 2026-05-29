// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"encoding/binary"
	"errors"
	"fmt"

	"gopkg.in/hraban/opus.v2"
)

// Decoder is a one-direction Opus decoder. Stateful; one per stream.
// Configured at construction time for a specific (sample_rate, channels,
// frame_ms) tuple.
//
// The device's mic stream is (16000, 1, 60) per its hello — that's what
// we use for incoming audio.
type Decoder struct {
	dec             *opus.Decoder
	sampleRate      int
	channels        int
	frameMS         int
	samplesPerFrame int
	pcmScratch      []int16
}

// NewDecoder constructs a decoder. Same sample-rate / frame-duration
// constraints as NewEncoder.
func NewDecoder(sampleRate, channels, frameMS int) (*Decoder, error) {
	if channels < 1 || channels > 2 {
		return nil, fmt.Errorf("audio: channels must be 1 or 2, got %d", channels)
	}
	dec, err := opus.NewDecoder(sampleRate, channels)
	if err != nil {
		return nil, fmt.Errorf("audio: NewDecoder: %w", err)
	}
	samples := sampleRate * frameMS / 1000
	if samples*1000 != sampleRate*frameMS {
		return nil, fmt.Errorf("audio: frame_ms=%d does not divide evenly into sample_rate=%d", frameMS, sampleRate)
	}
	return &Decoder{
		dec:             dec,
		sampleRate:      sampleRate,
		channels:        channels,
		frameMS:         frameMS,
		samplesPerFrame: samples,
		pcmScratch:      make([]int16, samples*channels),
	}, nil
}

// SamplesPerFrame returns the per-channel sample count one frame produces.
func (d *Decoder) SamplesPerFrame() int { return d.samplesPerFrame }

// SampleRate returns the configured sample rate.
func (d *Decoder) SampleRate() int { return d.sampleRate }

// ErrShortDecode is returned when an Opus packet decodes to fewer samples
// than expected (corrupted packet, FEC issues).
var ErrShortDecode = errors.New("audio: decoded fewer samples than expected")

// Decode runs one Opus packet through the decoder. Returns a slice of
// int16 PCM samples (interleaved if stereo) owned by the decoder; copy
// before the next Decode() call if you need to keep it.
//
// Pass an empty slice (or nil) to request packet-loss concealment for a
// missed frame — libopus will synthesize a frame from its internal state.
func (d *Decoder) Decode(opusBytes []byte) ([]int16, error) {
	n, err := d.dec.Decode(opusBytes, d.pcmScratch)
	if err != nil {
		return nil, fmt.Errorf("audio: decode: %w", err)
	}
	if n != d.samplesPerFrame {
		return nil, ErrShortDecode
	}
	return d.pcmScratch[:n*d.channels], nil
}

// DecodeAppend decodes opusBytes and appends the resulting little-endian
// PCM bytes to dst. Convenient when buffering many frames into a single
// PCM blob (e.g., for handing to whisper).
func (d *Decoder) DecodeAppend(dst []byte, opusBytes []byte) ([]byte, error) {
	pcm, err := d.Decode(opusBytes)
	if err != nil {
		return dst, err
	}
	for _, s := range pcm {
		dst = binary.LittleEndian.AppendUint16(dst, uint16(s))
	}
	return dst, nil
}
