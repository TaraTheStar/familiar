// SPDX-License-Identifier: AGPL-3.0-or-later

// Package asr implements speech-to-text via whisper.cpp.
//
// This wraps the whisper.cpp C API directly via cgo. The whisper.cpp
// source is vendored as a git submodule at third_party/whisper.cpp; the
// libwhisper.a + libggml*.a static libraries must be built before
// `go build` (see Makefile in the repo root).
//
// Models (ggml-*.bin files) are NOT bundled here — they're 75MB-3GB and
// loaded at runtime from a path on disk. Caller supplies the path; we
// embed the tiny model into the final binary via go:embed in a later step.
package asr

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/whisper.cpp/include
#cgo CFLAGS: -I${SRCDIR}/../../third_party/whisper.cpp/ggml/include
#cgo LDFLAGS: -L${SRCDIR}/../../third_party/whisper.cpp/build/src
#cgo LDFLAGS: -L${SRCDIR}/../../third_party/whisper.cpp/build/ggml/src
#cgo LDFLAGS: -L${SRCDIR}/../../third_party/whisper.cpp/build/ggml/src/ggml-cpu
#cgo LDFLAGS: -lwhisper -lggml -lggml-cpu -lggml-base -lstdc++ -lm -lpthread

#include <stdlib.h>
#include "whisper.h"

// Helper: build default params and force English transcription.
// Lives in C so we don't have to mirror the whole whisper_full_params struct
// across cgo. Returns the params by value; caller passes it back to
// whisper_full() directly.
static struct whisper_full_params stackend_default_params(int n_threads) {
    struct whisper_full_params p = whisper_full_default_params(WHISPER_SAMPLING_GREEDY);
    p.language       = "en";
    p.translate      = false;
    p.no_context     = true;
    p.print_progress = false;
    p.print_realtime = false;
    p.print_timestamps = false;
    p.print_special  = false;
    p.suppress_blank = true;
    p.single_segment = false;
    p.n_threads      = n_threads;
    return p;
}

// Route whisper.cpp/ggml logging into Go (see log.go) instead of raw
// stderr, which would corrupt the structured (JSON-per-line) log stream.
extern void stackendWhisperLogBridge(int level, char *text);
static void stackend_log_cb(enum ggml_log_level level, const char *text, void *user_data) {
    (void)user_data;
    stackendWhisperLogBridge((int)level, (char *)text);
}
static void stackend_install_log_callback(void) {
    whisper_log_set(stackend_log_cb, NULL);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

// Whisper holds a loaded model + state. One per server is enough — the
// inference itself is single-threaded per call but Transcribe holds a
// mutex so multiple sessions can share the model serially. Spinning up
// a second model just to parallelize would double RAM (the tiny model
// is 75MB so this is per-session-acceptable, but a single shared model
// is the conventional layout and good enough.)
type Whisper struct {
	ctx *C.struct_whisper_context
	mu  sync.Mutex
	cfg Config
}

// Config picks model + threading.
type Config struct {
	// ModelPath is the filesystem path to a ggml-*.bin file. Required.
	ModelPath string

	// Threads controls inference parallelism. 0 → runtime.NumCPU().
	Threads int
}

// Version returns the linked whisper.cpp version string.
func Version() string { return C.GoString(C.whisper_version()) }

// logCallbackOnce installs the whisper->slog log bridge exactly once,
// before the first model load (init is the chattiest phase).
var logCallbackOnce sync.Once

// New loads a model from disk. Returns an error if the model file is
// missing, malformed, or whisper internals can't initialize.
func New(cfg Config) (*Whisper, error) {
	if cfg.ModelPath == "" {
		return nil, errors.New("asr: ModelPath is empty")
	}
	logCallbackOnce.Do(func() { C.stackend_install_log_callback() })
	cPath := C.CString(cfg.ModelPath)
	defer C.free(unsafe.Pointer(cPath))

	params := C.whisper_context_default_params()
	ctx := C.whisper_init_from_file_with_params(cPath, params)
	if ctx == nil {
		return nil, fmt.Errorf("asr: whisper_init_from_file_with_params(%s) failed", cfg.ModelPath)
	}
	w := &Whisper{ctx: ctx, cfg: cfg}
	// Finalizer is a safety net; users should call Close explicitly.
	runtime.SetFinalizer(w, func(w *Whisper) { w.Close() })
	return w, nil
}

// Close frees the underlying whisper context. Subsequent Transcribe calls
// will panic — make sure no goroutine is using the Whisper instance.
func (w *Whisper) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx != nil {
		C.whisper_free(w.ctx)
		w.ctx = nil
	}
	runtime.SetFinalizer(w, nil)
}

// Transcribe runs whisper inference over the supplied PCM. The input
// MUST be 16kHz mono signed-16 PCM — that's the only rate whisper
// understands. Returns the concatenated text of all segments, trimmed
// of leading/trailing whitespace.
//
// Empty input returns ("", nil) without invoking whisper.
func (w *Whisper) Transcribe(pcm []int16) (string, error) {
	if len(pcm) == 0 {
		return "", nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx == nil {
		return "", errors.New("asr: whisper already closed")
	}

	// whisper takes float32 in [-1, 1]; convert from s16.
	samples := make([]float32, len(pcm))
	for i, s := range pcm {
		samples[i] = float32(s) / 32768.0
	}

	threads := w.cfg.Threads
	if threads <= 0 {
		threads = runtime.NumCPU()
	}
	params := C.stackend_default_params(C.int(threads))

	rc := C.whisper_full(
		w.ctx,
		params,
		(*C.float)(unsafe.Pointer(&samples[0])),
		C.int(len(samples)),
	)
	if rc != 0 {
		return "", fmt.Errorf("asr: whisper_full rc=%d", int(rc))
	}

	n := int(C.whisper_full_n_segments(w.ctx))
	var sb strings.Builder
	for i := 0; i < n; i++ {
		seg := C.whisper_full_get_segment_text(w.ctx, C.int(i))
		sb.WriteString(C.GoString(seg))
	}
	return strings.TrimSpace(sb.String()), nil
}
