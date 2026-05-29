// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"encoding/binary"
	"io"
)

// PCMFramer reads raw 16-bit little-endian PCM from r and yields complete
// frames of `samples` int16 values each. The last (partial) frame is
// zero-padded so the encoder never sees a short input.
//
// Construct one PCMFramer per Reader and call Next() until it returns false.
// Errors are surfaced via Err() after Next() returns false.
type PCMFramer struct {
	r       io.Reader
	samples int
	buf     []int16
	bytes   []byte
	err     error
	done    bool
}

// NewPCMFramer wraps r. `samples` is the per-frame sample count expected by
// the consumer (typically Encoder.SamplesPerFrame() for mono).
func NewPCMFramer(r io.Reader, samples int) *PCMFramer {
	return &PCMFramer{
		r:       r,
		samples: samples,
		buf:     make([]int16, samples),
		bytes:   make([]byte, samples*2),
	}
}

// Next reads the next frame into the internal buffer. Returns true if a
// frame was produced (full or zero-padded), false on EOF or error.
func (f *PCMFramer) Next() bool {
	if f.done {
		return false
	}
	n, err := io.ReadFull(f.r, f.bytes)
	switch {
	case err == nil:
		// full frame
	case err == io.EOF:
		f.done = true
		return false
	case err == io.ErrUnexpectedEOF:
		// partial frame — zero-pad. n is bytes read; rest stays zero.
		for i := n; i < len(f.bytes); i++ {
			f.bytes[i] = 0
		}
		f.done = true
	default:
		f.err = err
		f.done = true
		return false
	}
	// Decode little-endian s16 into int16 slice.
	for i := 0; i < f.samples; i++ {
		f.buf[i] = int16(binary.LittleEndian.Uint16(f.bytes[i*2:]))
	}
	return true
}

// Frame returns the most recent frame. Only valid between successful Next()
// and the next Next() call.
func (f *PCMFramer) Frame() []int16 { return f.buf }

// Err returns the first non-EOF read error, or nil.
func (f *PCMFramer) Err() error { return f.err }
