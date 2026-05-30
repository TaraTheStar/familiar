// SPDX-License-Identifier: AGPL-3.0-or-later

package protov2

import (
	"encoding/json"
	"testing"
)

// FuzzDecode feeds arbitrary bytes to Decode to catch dispatcher panics and
// non-error crashes, and checks one invariant: a successfully decoded message
// must re-encode to JSON and re-decode to the same type. Decode must never
// panic and must never return both a nil message and a nil error.
//
// Run: GOWORK=off go test ./internal/protov2 -run x -fuzz FuzzDecode
func FuzzDecode(f *testing.F) {
	// Seed with every committed exemplar plus the known edge cases, so the
	// fuzzer starts from valid frames and mutates outward.
	for _, ex := range exemplars() {
		if body, err := json.Marshal(ex.value); err == nil {
			f.Add(body)
		}
	}
	for _, s := range []string{
		``, `{}`, `{"type":""}`, `{"type":"hello"}`, `{not json`,
		`{"type":"wake","score":"not-a-number"}`,
		`{"type":"audio_credit","frames":-5}`,
		`[]`, `null`, `"just a string"`, `{"type":123}`,
	} {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := Decode(data)
		if err != nil {
			if msg != nil {
				t.Fatalf("Decode returned both a message (%#v) and an error (%v)", msg, err)
			}
			return // a decode error is a legitimate outcome for garbage input
		}
		if msg == nil {
			t.Fatalf("Decode returned nil message and nil error for %q", data)
		}

		// Unknown carries raw bytes and need not round-trip structurally.
		if _, ok := msg.(Unknown); ok {
			return
		}

		// A decoded typed message must re-encode and re-decode to the same
		// concrete type — the dispatcher and the structs must agree on `type`.
		reencoded, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("re-encode %T: %v", msg, err)
		}
		redecoded, err := Decode(reencoded)
		if err != nil {
			t.Fatalf("re-decode %T (%s): %v", msg, reencoded, err)
		}
		if got, want := typeName(redecoded), typeName(msg); got != want {
			t.Fatalf("round-trip changed type: %s -> %s\noriginal: %q\nre-encoded: %s", want, got, data, reencoded)
		}
	})
}

func typeName(m Message) string {
	switch m.(type) {
	case ClientHello:
		return "ClientHello"
	case ListenStart:
		return "ListenStart"
	case ListenStop:
		return "ListenStop"
	case Wake:
		return "Wake"
	case Abort:
		return "Abort"
	case Telemetry:
		return "Telemetry"
	case AudioCredit:
		return "AudioCredit"
	case Goodbye:
		return "Goodbye"
	case Error:
		return "Error"
	case ToolList:
		return "ToolList"
	case ToolCall:
		return "ToolCall"
	case Unknown:
		return "Unknown"
	default:
		return "?"
	}
}
