// SPDX-License-Identifier: AGPL-3.0-or-later
//go:build realasr

// Tests in this file require a real whisper model (75MB+) and exercise the
// full inference pipeline. They're gated behind the `realasr` build tag so
// the default test suite stays fast and self-contained.
//
// Run them via `make test-real-asr` after `make tiny-model`.

package asr

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTranscribeJFK loads the real tiny.en model and transcribes the JFK
// "ask not what your country can do for you" clip bundled with
// whisper.cpp. We assert the transcript contains the word "country" —
// that's enough to confirm the inference pipeline produces something
// English-shaped rather than garbage.
func TestTranscribeJFK(t *testing.T) {
	modelPath := os.Getenv("WHISPER_TINY_EN_MODEL")
	if modelPath == "" {
		t.Skip("WHISPER_TINY_EN_MODEL not set; run via `make test-real-asr`")
	}
	if _, err := os.Stat(modelPath); err != nil {
		t.Skipf("model not present at %s: %v", modelPath, err)
	}

	root := findRepoRoot(t)
	wavPath := filepath.Join(root, "third_party", "whisper.cpp", "samples", "jfk.wav")

	pcm, err := readWAV16k(wavPath)
	if err != nil {
		t.Fatalf("readWAV16k: %v", err)
	}
	t.Logf("loaded %d samples (%.2fs at 16kHz)", len(pcm), float64(len(pcm))/16000)

	w, err := New(Config{ModelPath: modelPath, Threads: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	got, err := w.Transcribe(pcm)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	t.Logf("transcript: %q", got)

	// Loose assertion: tiny.en is small, transcripts can vary slightly.
	// "country" is the most distinctive word in the clip.
	if !strings.Contains(strings.ToLower(got), "country") {
		t.Errorf("transcript missing 'country': %q", got)
	}
}

// readWAV16k parses a minimal canonical PCM WAV (mono, 16-bit, 16kHz)
// and returns the samples. Whisper.cpp ships samples in this format.
func readWAV16k(path string) ([]int16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 44 {
		return nil, fmt.Errorf("wav: too short (%d bytes)", len(data))
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, fmt.Errorf("wav: not a RIFF/WAVE file")
	}
	if string(data[12:16]) != "fmt " {
		return nil, fmt.Errorf("wav: missing fmt chunk")
	}
	audioFmt := binary.LittleEndian.Uint16(data[20:22])
	chans := binary.LittleEndian.Uint16(data[22:24])
	rate := binary.LittleEndian.Uint32(data[24:28])
	bits := binary.LittleEndian.Uint16(data[34:36])
	if audioFmt != 1 || chans != 1 || rate != 16000 || bits != 16 {
		return nil, fmt.Errorf("wav: need PCM mono 16kHz 16-bit; got fmt=%d ch=%d rate=%d bits=%d",
			audioFmt, chans, rate, bits)
	}
	// Locate the 'data' chunk header. Standard PCM WAVs have it at offset 36,
	// but well-behaved files may include extra format-chunk bytes.
	off := 36
	for off+8 <= len(data) {
		if string(data[off:off+4]) == "data" {
			size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
			start := off + 8
			end := start + size
			if end > len(data) {
				end = len(data)
			}
			samples := make([]int16, (end-start)/2)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(data[start+i*2:]))
			}
			return samples, nil
		}
		// Skip this chunk; size field is at off+4.
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		off += 8 + size
	}
	return nil, fmt.Errorf("wav: no data chunk found")
}
