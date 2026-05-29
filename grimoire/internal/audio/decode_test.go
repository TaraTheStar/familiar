// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"math"
	"testing"
)

func TestDecoderDimensionsForMic(t *testing.T) {
	// Mic stream: 16kHz mono 60ms.
	dec, err := NewDecoder(16000, 1, 60)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if got, want := dec.SamplesPerFrame(), 960; got != want {
		t.Errorf("SamplesPerFrame=%d, want %d", got, want)
	}
}

func TestRoundtripPreservesSineWave(t *testing.T) {
	// Encode a sine wave through the encoder, decode through the decoder,
	// check RMS energy is preserved. Same idea as the encoder test but
	// proving the encoder + decoder agree.
	const (
		sampleRate = 16000
		frameMS    = 60
		freqHz     = 440
		amplitude  = 8000
	)
	enc, err := NewEncoder(sampleRate, 1, frameMS)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	dec, err := NewDecoder(sampleRate, 1, frameMS)
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}

	frame := make([]int16, enc.SamplesPerFrame())
	for i := range frame {
		t := float64(i) / sampleRate
		frame[i] = int16(amplitude * math.Sin(2*math.Pi*freqHz*t))
	}

	var lastDecoded []int16
	for i := 0; i < 3; i++ {
		opusBytes, err := enc.Encode(frame)
		if err != nil {
			t.Fatalf("Encode %d: %v", i, err)
		}
		decoded, err := dec.Decode(opusBytes)
		if err != nil {
			t.Fatalf("Decode %d: %v", i, err)
		}
		// Copy because the decoder reuses its scratch slice.
		lastDecoded = append([]int16(nil), decoded...)
	}

	inRMS := rms(frame)
	outRMS := rms(lastDecoded)
	ratio := outRMS / inRMS
	if ratio < 0.5 || ratio > 1.5 {
		t.Errorf("RMS ratio out/in = %.3f; expected ~1.0", ratio)
	}
}

func TestDecodeAppend(t *testing.T) {
	enc, _ := NewEncoder(16000, 1, 60)
	dec, _ := NewDecoder(16000, 1, 60)

	frame := make([]int16, enc.SamplesPerFrame())
	for i := range frame {
		frame[i] = int16(i % 10000)
	}

	opusBytes, err := enc.Encode(frame)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := dec.DecodeAppend(nil, opusBytes)
	if err != nil {
		t.Fatalf("DecodeAppend: %v", err)
	}
	// 960 samples * 2 bytes = 1920 bytes
	if len(out) != enc.SamplesPerFrame()*2 {
		t.Errorf("DecodeAppend len=%d, want %d", len(out), enc.SamplesPerFrame()*2)
	}
}
