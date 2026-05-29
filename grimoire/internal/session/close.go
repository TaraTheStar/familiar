// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "github.com/coder/websocket"

// closeNormal sends a normal-closure frame to the device. The device's
// OnAudioChannelClosed callback transitions it back to idle; firmware's
// lazy-WS layer will reconnect on the next wake word.
func (s *Session) closeNormal() error {
	return s.conn.Close(websocket.StatusNormalClosure, "goodbye")
}
