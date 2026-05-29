// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/coder/websocket"
)

// writeJSON marshals v and sends it as a text WebSocket frame. coder/websocket
// already serializes concurrent writes via its internal write mutex, so we
// don't need our own lock at this layer.
func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
