// SPDX-License-Identifier: AGPL-3.0-or-later

package asr

import "C"

import (
	"log/slog"
	"strings"
	"sync"
)

// whisper.cpp/ggml write their logs straight to stderr by default, which
// corrupts the one-JSON-object-per-line log stream the server emits. The
// callback installed by New() (stackend_install_log_callback in whisper.go)
// routes every C-side log line here instead, where it is re-emitted via
// slog under the "whisper" message.
//
// ggml emits partial lines (a final fragment then GGML_LOG_LEVEL_CONT
// continuations), so fragments are buffered until a newline arrives.

// ggml_log_level values (ggml.h). DEBUG=1 and INFO=2 both land in the
// switch default below.
const (
	ggmlLogLevelInfo  = 2
	ggmlLogLevelWarn  = 3
	ggmlLogLevelError = 4
	ggmlLogLevelCont  = 5
)

var whisperLogMu sync.Mutex
var whisperLogBuf strings.Builder
var whisperLogLevel int = ggmlLogLevelInfo

//export stackendWhisperLogBridge
func stackendWhisperLogBridge(level C.int, text *C.char) {
	s := C.GoString(text)
	if s == "" {
		return
	}

	whisperLogMu.Lock()
	defer whisperLogMu.Unlock()

	// CONT continues the previous fragment at its original level.
	if int(level) != ggmlLogLevelCont {
		whisperLogLevel = int(level)
	}
	whisperLogBuf.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		return // fragment; wait for the rest of the line
	}

	lines := whisperLogBuf.String()
	whisperLogBuf.Reset()
	for _, line := range strings.Split(strings.TrimRight(lines, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Model-load banners and other info-level chatter go to Debug so a
		// default info-level deployment stays quiet; problems stay visible.
		switch whisperLogLevel {
		case ggmlLogLevelError:
			slog.Error("whisper", "line", line)
		case ggmlLogLevelWarn:
			slog.Warn("whisper", "line", line)
		default:
			slog.Debug("whisper", "line", line)
		}
	}
}
