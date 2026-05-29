// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"gopkg.in/hraban/opus.v2"
)

func TestEncoderDimensionsForStackChan(t *testing.T) {
	// The server hello we advertise: 24kHz mono 60ms frames.
	enc, err := NewEncoder(24000, 1, 60)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if got, want := enc.SamplesPerFrame(), 1440; got != want {
		t.Errorf("SamplesPerFrame=%d, want %d", got, want)
	}
	if got, want := enc.FrameBytes(), 2880; got != want {
		t.Errorf("FrameBytes=%d, want %d", got, want)
	}
}

func TestEncoderRejectsShortFrame(t *testing.T) {
	enc, err := NewEncoder(24000, 1, 60)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	_, err = enc.Encode(make([]int16, 100)) // wrong size
	if err != ErrShortFrame {
		t.Errorf("got err=%v, want ErrShortFrame", err)
	}
}

func TestEncoderRoundtripPreservesEnergy(t *testing.T) {
	// Encode → decode a sine wave and confirm RMS energy is in the same
	// ballpark. Opus is lossy so we don't get bit-exact, but voice-quality
	// energy should survive within a couple dB.
	const (
		sampleRate = 24000
		frameMS    = 60
		freqHz     = 440 // A4
		amplitude  = 8000
	)
	enc, err := NewEncoder(sampleRate, 1, frameMS)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	dec, err := opus.NewDecoder(sampleRate, 1)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}

	frame := make([]int16, enc.SamplesPerFrame())
	for i := range frame {
		// One frame's worth of 440Hz sine at amplitude 8000.
		t := float64(i) / sampleRate
		frame[i] = int16(amplitude * math.Sin(2*math.Pi*freqHz*t))
	}

	// Encode → decode a few frames so the encoder warms up; the first
	// frame of any Opus stream has reduced quality.
	var lastDecoded []int16
	for i := 0; i < 3; i++ {
		opusBytes, err := enc.Encode(frame)
		if err != nil {
			t.Fatalf("Encode frame %d: %v", i, err)
		}
		out := make([]int16, enc.SamplesPerFrame())
		n, err := dec.Decode(opusBytes, out)
		if err != nil {
			t.Fatalf("Decode frame %d: %v", i, err)
		}
		lastDecoded = out[:n]
	}

	inRMS := rms(frame)
	outRMS := rms(lastDecoded)
	ratio := outRMS / inRMS
	if ratio < 0.5 || ratio > 1.5 {
		t.Errorf("RMS ratio out/in = %.3f (input=%.1f, output=%.1f); expected ~1.0", ratio, inRMS, outRMS)
	}
}

func TestPCMFramerSplits(t *testing.T) {
	// 1500 samples → at 600-sample frames = 2 full + 1 zero-padded frame.
	src := make([]int16, 1500)
	for i := range src {
		src[i] = int16(i)
	}
	raw := pcmBytes(src)

	f := NewPCMFramer(bytes.NewReader(raw), 600)

	frames := 0
	for f.Next() {
		frames++
	}
	if f.Err() != nil {
		t.Fatalf("framer err: %v", f.Err())
	}
	if frames != 3 {
		t.Errorf("frames=%d, want 3", frames)
	}
}

func TestPCMFramerEmpty(t *testing.T) {
	f := NewPCMFramer(bytes.NewReader(nil), 100)
	if f.Next() {
		t.Error("expected Next() false on empty input")
	}
}

// Helpers ---------------------------------------------------------------------

func rms(s []int16) float64 {
	var sum float64
	for _, v := range s {
		x := float64(v)
		sum += x * x
	}
	return math.Sqrt(sum / float64(len(s)))
}

func pcmBytes(s []int16) []byte {
	out := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}
