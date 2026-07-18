package session

import (
	"encoding/json"
	"log/slog"
)

// jsonAttr embeds s as raw JSON when it is a valid JSON value, so the JSON
// log handler emits a nested object instead of an escaped string. Invalid or
// empty payloads fall back to a plain string attr.
func jsonAttr(key, s string) slog.Attr {
	if s != "" && json.Valid([]byte(s)) {
		return slog.Any(key, json.RawMessage(s))
	}
	return slog.String(key, s)
}
