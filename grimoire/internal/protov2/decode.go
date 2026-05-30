// SPDX-License-Identifier: AGPL-3.0-or-later

package protov2

import (
	"encoding/json"
	"fmt"
)

// Message is the closed sum type of all device → server JSON messages Decode
// can produce. Bidirectional types (Goodbye, Error, ToolList, ToolCall) are
// members because the device side of those exchanges decodes here too.
type Message interface {
	isV2Message()
}

func (ClientHello) isV2Message() {}
func (ListenStart) isV2Message() {}
func (ListenStop) isV2Message()  {}
func (Wake) isV2Message()        {}
func (Abort) isV2Message()       {}
func (Telemetry) isV2Message()   {}
func (AudioCredit) isV2Message() {}
func (Goodbye) isV2Message()     {}
func (Error) isV2Message()       {}
func (ToolList) isV2Message()    {}
func (ToolCall) isV2Message()    {}

// Unknown wraps a message whose `type` is outside the v2 catalog. Keeping the
// raw bytes lets callers log it without breaking forward compatibility.
type Unknown struct {
	Type string
	Raw  json.RawMessage
}

func (Unknown) isV2Message() {}

// ErrInvalidJSON is returned when the bytes are not valid JSON.
type ErrInvalidJSON struct{ Err error }

func (e ErrInvalidJSON) Error() string { return "protov2: invalid JSON: " + e.Err.Error() }
func (e ErrInvalidJSON) Unwrap() error { return e.Err }

// ErrMissingType is returned when the JSON parses but has no `type` field.
var ErrMissingType = fmt.Errorf("protov2: missing 'type' field")

// Decode parses an incoming WebSocket text frame into a typed Message. Unknown
// types are surfaced as Unknown (not an error) so callers log and continue.
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
		return decodeInto[ClientHello](data)
	case "listen_start":
		return decodeInto[ListenStart](data)
	case "listen_stop":
		return decodeInto[ListenStop](data)
	case "wake":
		return decodeInto[Wake](data)
	case "abort":
		return decodeInto[Abort](data)
	case "telemetry":
		return decodeInto[Telemetry](data)
	case "audio_credit":
		return decodeInto[AudioCredit](data)
	case "goodbye":
		return decodeInto[Goodbye](data)
	case "error":
		return decodeInto[Error](data)
	case "tool_list":
		return decodeInto[ToolList](data)
	case "tool_call":
		return decodeInto[ToolCall](data)
	default:
		return Unknown{Type: head.Type, Raw: append(json.RawMessage(nil), data...)}, nil
	}
}

// decodeInto unmarshals data into T and returns it as a Message, wrapping any
// JSON error in ErrInvalidJSON. The type parameter must implement Message;
// every catalog type does.
func decodeInto[T Message](data []byte) (Message, error) {
	var m T
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, ErrInvalidJSON{Err: err}
	}
	return m, nil
}
