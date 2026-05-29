// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol

import (
	"encoding/json"
	"fmt"
)

// Message is the closed sum type of all device → server JSON messages.
type Message interface {
	isMessage()
}

func (ClientHello) isMessage() {}
func (Listen) isMessage()      {}
func (Abort) isMessage()       {}
func (MCP) isMessage()         {}
func (Event) isMessage()       {}

// Unknown wraps a message whose `type` is not in the v1 catalog. Keeping the
// raw bytes lets us log it without crashing on forward-compat surprises.
type Unknown struct {
	Type string
	Raw  json.RawMessage
}

func (Unknown) isMessage() {}

// ErrInvalidJSON is returned when the bytes are not valid JSON.
type ErrInvalidJSON struct{ Err error }

func (e ErrInvalidJSON) Error() string { return "protocol: invalid JSON: " + e.Err.Error() }
func (e ErrInvalidJSON) Unwrap() error { return e.Err }

// ErrMissingType is returned when the JSON parses but has no `type` field.
var ErrMissingType = fmt.Errorf("protocol: missing 'type' field")

// Decode parses an incoming WebSocket text frame into a typed Message.
// Unknown types are surfaced as Unknown (not an error) so callers can log
// and continue without breaking forward compatibility.
func Decode(data []byte) (Message, error) {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, ErrInvalidJSON{Err: err}
	}
	if head.Type == "" {
		return nil, ErrMissingType
	}

	switch head.Type {
	case "hello":
		var m ClientHello
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, ErrInvalidJSON{Err: err}
		}
		return m, nil
	case "listen":
		var m Listen
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, ErrInvalidJSON{Err: err}
		}
		return m, nil
	case "abort":
		var m Abort
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, ErrInvalidJSON{Err: err}
		}
		return m, nil
	case "mcp":
		var m MCP
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, ErrInvalidJSON{Err: err}
		}
		return m, nil
	case "event":
		var m Event
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, ErrInvalidJSON{Err: err}
		}
		return m, nil
	default:
		return Unknown{Type: head.Type, Raw: append(json.RawMessage(nil), data...)}, nil
	}
}
