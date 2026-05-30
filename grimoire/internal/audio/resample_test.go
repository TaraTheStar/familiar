// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func readAllSamples(t *testing.T, r io.Reader) []int16 {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("odd byte count %d", len(raw))
	}
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return out
}

func TestResampleEqualRateIsPassthrough(t *testing.T) {
	src := bytes.NewReader([]byte{1, 2, 3, 4})
	if got := NewResampleReader(src, 16000, 16000); got != src {
		t.Fatalf("equal rates should return the source unchanged")
	}
}

func TestResample24kTo16kLength(t *testing.T) {
	// 2400 input samples @24k → ~1600 @16k (2/3 ratio).
	in := make([]int16, 2400)
	for i := range in {
		in[i] = 1000 // constant signal stays constant through linear interp
	}
	r := NewResampleReader(bytes.NewReader(pcmBytes(in)), 24000, 16000)
	out := readAllSamples(t, r)

	if d := len(out) - 1600; d < -2 || d > 2 {
		t.Fatalf("expected ~1600 output samples, got %d", len(out))
	}
	for i, s := range out {
		if s < 998 || s > 1002 {
			t.Fatalf("sample %d = %d, want ~1000 (constant signal)", i, s)
		}
	}
}

func TestResampleLinearMidpoint(t *testing.T) {
	// A clean ramp: linear interpolation must land on the analytic line.
	in := make([]int16, 600)
	for i := range in {
		in[i] = int16(i * 10)
	}
	r := NewResampleReader(bytes.NewReader(pcmBytes(in)), 24000, 16000)
	out := readAllSamples(t, r)
	// Output sample k samples input position k*1.5; value ≈ (k*1.5)*10.
	for k, got := range out {
		want := float64(k) * 1.5 * 10
		if diff := float64(got) - want; diff < -6 || diff > 6 {
			t.Fatalf("out[%d]=%d, want ~%.0f", k, got, want)
		}
	}
}

// shortReader hands back one byte at a time to exercise the streaming /
// sample-straddling-a-read path.
type shortReader struct {
	data []byte
	pos  int
}

func (s *shortReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = s.data[s.pos]
	s.pos++
	return 1, nil
}

func TestResampleHandlesByteAtATimeSource(t *testing.T) {
	in := make([]int16, 300)
	for i := range in {
		in[i] = int16(i)
	}
	whole := NewResampleReader(bytes.NewReader(pcmBytes(in)), 24000, 16000)
	want := readAllSamples(t, whole)

	dribble := NewResampleReader(&shortReader{data: pcmBytes(in)}, 24000, 16000)
	got := readAllSamples(t, dribble)

	if len(got) != len(want) {
		t.Fatalf("length mismatch: dribbled %d vs whole %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: dribbled %d vs whole %d", i, got[i], want[i])
		}
	}
}
