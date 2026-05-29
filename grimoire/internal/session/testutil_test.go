// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"testing"

	"github.com/TaraTheStar/familiar/grimoire/internal/audio"
)

// newSendEncoder builds an Opus encoder matching the mic-audio side of
// the contract (16kHz mono 60ms). Used by tests that need to ship valid
// Opus packets through the server.
func newSendEncoder(t *testing.T) *audio.Encoder {
	t.Helper()
	e, err := audio.NewEncoder(16000, 1, 60)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	return e
}
