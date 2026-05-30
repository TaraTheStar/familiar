// SPDX-License-Identifier: AGPL-3.0-or-later

package v2client

import (
	"encoding/binary"
	"fmt"
	"os"
)

// ReadWAV reads a 16-bit little-endian PCM WAV file, returning the raw sample
// bytes, sample rate, and channel count. Exported for the CLI, which validates
// the format before sending.
func ReadWAV(path string) (pcm []byte, sampleRate, channels int, err error) {
	return readWAV(path)
}

// readWAV reads a 16-bit little-endian PCM WAV file, returning the raw sample
// bytes (the "data" chunk) and its sample rate. It walks the RIFF chunk list
// rather than assuming a fixed 44-byte header, so files with extra leading
// chunks (LIST/fact/etc.) still parse. Only PCM (format 1), 16-bit, is
// supported — that's what the mic stream uses.
func readWAV(path string) (pcm []byte, sampleRate, channels int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, 0, err
	}
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, 0, 0, fmt.Errorf("v2client: %s is not a RIFF/WAVE file", path)
	}

	var bits int
	off := 12
	for off+8 <= len(raw) {
		id := string(raw[off : off+4])
		size := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		body := off + 8
		if body+size > len(raw) {
			size = len(raw) - body // tolerate a truncated final chunk
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, 0, 0, fmt.Errorf("v2client: short fmt chunk in %s", path)
			}
			format := binary.LittleEndian.Uint16(raw[body : body+2])
			channels = int(binary.LittleEndian.Uint16(raw[body+2 : body+4]))
			sampleRate = int(binary.LittleEndian.Uint32(raw[body+4 : body+8]))
			bits = int(binary.LittleEndian.Uint16(raw[body+14 : body+16]))
			if format != 1 || bits != 16 {
				return nil, 0, 0, fmt.Errorf("v2client: %s must be 16-bit PCM (format=%d bits=%d)", path, format, bits)
			}
		case "data":
			pcm = raw[body : body+size]
		}
		// Chunks are word-aligned: an odd size is followed by a pad byte.
		off = body + size + (size & 1)
	}
	if sampleRate == 0 {
		return nil, 0, 0, fmt.Errorf("v2client: no fmt chunk in %s", path)
	}
	if pcm == nil {
		return nil, 0, 0, fmt.Errorf("v2client: no data chunk in %s", path)
	}
	return pcm, sampleRate, channels, nil
}

// writeWAV writes 16-bit little-endian mono PCM as a canonical 44-byte-header
// WAV file — the inverse of readWAV, used to dump the server's decoded TTS for
// listening.
func writeWAV(path string, pcm []byte, sampleRate int) error {
	const (
		numChannels   = 1
		bitsPerSample = 16
	)
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	dataLen := len(pcm)

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var hdr [44]byte
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(36+dataLen))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)
	binary.LittleEndian.PutUint16(hdr[20:22], 1)
	binary.LittleEndian.PutUint16(hdr[22:24], numChannels)
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(hdr[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(hdr[34:36], bitsPerSample)
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], uint32(dataLen))

	if _, err := f.Write(hdr[:]); err != nil {
		return err
	}
	_, err = f.Write(pcm)
	return err
}
