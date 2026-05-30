// SPDX-License-Identifier: AGPL-3.0-or-later

package audio

import (
	"encoding/binary"
	"io"
	"math"
)

// NewResampleReader wraps a 16-bit little-endian mono PCM stream and resamples
// it from inRate to outRate with linear interpolation. When the rates are equal
// it returns src unchanged (zero cost).
//
// It streams: each Read pulls only as much source audio as it needs to fill the
// caller's buffer, so it preserves the incremental delivery the TTS pacing
// relies on — no buffer-the-whole-utterance latency.
//
// Linear interpolation (no anti-alias FIR) is deliberate. The only caller is
// Kokoro's fixed 24kHz → the device's native 16kHz output, played on the
// StackChan's band-limited speaker, where a heavier filter would be inaudible.
// The point is to move the downsample off the ESP32's audio core, where it was
// competing with wakenet + AEC during playback and starving wake-word
// detection (breaking barge-in). The server has CPU to spare.
func NewResampleReader(src io.Reader, inRate, outRate int) io.Reader {
	if inRate == outRate || inRate <= 0 || outRate <= 0 {
		return src
	}
	return &resampleReader{
		src:     src,
		step:    float64(inRate) / float64(outRate),
		scratch: make([]byte, 4096),
	}
}

type resampleReader struct {
	src  io.Reader
	step float64 // input samples advanced per output sample

	scratch []byte // read buffer pulled from src
	buf     []byte // source bytes read but not yet consumed
	off     int    // consume offset into buf
	srcEOF  bool
	srcErr  error

	inited    bool
	exhausted bool // all source consumed; further Reads return EOF
	cur, next int16
	frac      float64 // position in [0,1) between cur and next
}

// nextInput returns the next source sample, or false once the source is drained.
func (r *resampleReader) nextInput() (int16, bool) {
	for len(r.buf)-r.off < 2 {
		if r.srcEOF {
			return 0, false
		}
		if r.off > 0 { // compact consumed bytes
			n := copy(r.buf, r.buf[r.off:])
			r.buf = r.buf[:n]
			r.off = 0
		}
		n, err := r.src.Read(r.scratch)
		if n > 0 {
			r.buf = append(r.buf, r.scratch[:n]...)
		}
		if err != nil {
			r.srcEOF = true
			if err != io.EOF {
				r.srcErr = err
			}
		}
	}
	s := int16(binary.LittleEndian.Uint16(r.buf[r.off : r.off+2]))
	r.off += 2
	return s, true
}

func (r *resampleReader) Read(p []byte) (int, error) {
	if !r.inited {
		c, ok := r.nextInput()
		if !ok {
			return 0, r.eof()
		}
		r.cur = c
		if n, ok := r.nextInput(); ok {
			r.next = n
		} else {
			r.next = c // single sample: emit it, then drain
			r.exhausted = true
		}
		r.inited = true
	}

	written := 0
	for written+2 <= len(p) {
		// Advance the window until the output position sits between cur and
		// next (frac < 1), or the source runs dry.
		for r.frac >= 1 && !r.exhausted {
			n, ok := r.nextInput()
			if !ok {
				r.exhausted = true
				break
			}
			r.cur = r.next
			r.next = n
			r.frac--
		}
		if r.frac >= 1 && r.exhausted {
			break // nothing left to interpolate toward
		}
		y := float64(r.cur) + (float64(r.next)-float64(r.cur))*r.frac
		binary.LittleEndian.PutUint16(p[written:], uint16(int16(math.Round(y))))
		written += 2
		r.frac += r.step
	}

	if written == 0 {
		if r.frac >= 1 && r.exhausted {
			return 0, r.eof()
		}
		return 0, nil // caller buffer < 2 bytes; it will retry
	}
	return written, nil
}

func (r *resampleReader) eof() error {
	if r.srcErr != nil {
		return r.srcErr
	}
	return io.EOF
}
