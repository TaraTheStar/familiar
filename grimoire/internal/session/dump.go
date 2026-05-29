// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

// writeWAV writes 16-bit little-endian mono PCM as a canonical 44-byte-header
// WAV file. Diagnostics only — used when DebugDumpDir is set so we can play
// back exactly what the server captured and fed to whisper.
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
	binary.LittleEndian.PutUint32(hdr[16:20], 16) // PCM fmt chunk size
	binary.LittleEndian.PutUint16(hdr[20:22], 1)  // PCM
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

// dumpTurn writes the captured PCM to DebugDumpDir as a WAV, returning the
// path written (or "" if dumping is disabled). Best-effort; errors are logged
// by the caller.
func (s *Session) dumpTurn(pcm []byte) (string, error) {
	if s.cfg.DebugDumpDir == "" {
		return "", nil
	}
	seq := s.turnSeq.Add(1)
	name := fmt.Sprintf("turn-%s-%03d.wav", s.sessionID, seq)
	path := filepath.Join(s.cfg.DebugDumpDir, name)
	return path, writeWAV(path, pcm, s.cfg.MicAudio.SampleRate)
}
