// SPDX-License-Identifier: AGPL-3.0-or-later

package v2client

import (
	"encoding/json"
	"fmt"

	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
)

// decodeServerFrame parses a server → device text frame into its concrete
// protov2 struct. It is the device-side mirror of protov2.Decode (which only
// covers the device → server direction): the reference client lives on the
// receiving end of transcript/display/caption/audio_*/alert/system, plus the
// bidirectional hello/goodbye/error/tool_list/tool_call. Returns the decoded
// value as any; callers type-switch on it.
func decodeServerFrame(data []byte) (any, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("v2client: invalid JSON frame: %w", err)
	}
	if head.Type == "" {
		return nil, fmt.Errorf("v2client: frame missing 'type'")
	}
	switch head.Type {
	case "hello":
		return unmarshalAs[protov2.ServerHello](data)
	case "transcript":
		return unmarshalAs[protov2.Transcript](data)
	case "audio_begin":
		return unmarshalAs[protov2.AudioBegin](data)
	case "audio_end":
		return unmarshalAs[protov2.AudioEnd](data)
	case "audio_cancel":
		return unmarshalAs[protov2.AudioCancel](data)
	case "caption":
		return unmarshalAs[protov2.Caption](data)
	case "display":
		return unmarshalAs[protov2.Display](data)
	case "alert":
		return unmarshalAs[protov2.Alert](data)
	case "system":
		return unmarshalAs[protov2.System](data)
	case "goodbye":
		return unmarshalAs[protov2.Goodbye](data)
	case "error":
		return unmarshalAs[protov2.Error](data)
	case "tool_list":
		return unmarshalAs[protov2.ToolList](data)
	case "tool_call":
		return unmarshalAs[protov2.ToolCall](data)
	default:
		// Forward-compatible: unknown types are surfaced for logging, not errors.
		return unknownFrame{Type: head.Type}, nil
	}
}

type unknownFrame struct{ Type string }

func unmarshalAs[T any](data []byte) (any, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("v2client: decode %T: %w", v, err)
	}
	return v, nil
}
