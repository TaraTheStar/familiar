// SPDX-License-Identifier: AGPL-3.0-or-later

package asr

import (
	"path/filepath"
	"runtime"
	"testing"
)

// findRepoRoot walks up from this source file to find the go-stackend root
// (the directory containing third_party/whisper.cpp). Used so tests can
// reference vendored test fixtures without absolute paths.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	// internal/asr/whisper_test.go → repo root is two levels up.
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// TestVersionString just confirms the linkage works and we can call into
// whisper.cpp at all.
func TestVersionString(t *testing.T) {
	v := Version()
	if v == "" {
		t.Fatal("Version() returned empty")
	}
	t.Logf("whisper.cpp version: %s", v)
}

// TestLoadMockModel uses the structural test model bundled with
// whisper.cpp (~575KB). It loads, transcribes a short silent buffer
// without crashing, and frees. Confirms the full lifecycle binds
// correctly without needing a real 75MB model download.
func TestLoadMockModel(t *testing.T) {
	root := findRepoRoot(t)
	mockPath := filepath.Join(root, "third_party", "whisper.cpp",
		"models", "for-tests-ggml-tiny.bin")

	w, err := New(Config{ModelPath: mockPath, Threads: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	// 1 second of silence at 16kHz.
	silence := make([]int16, 16000)
	// We don't assert on the output — the mock model produces garbage.
	// The contract being tested is "no crash, returns a string and nil
	// error."
	out, err := w.Transcribe(silence)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	t.Logf("mock model output: %q", out)
}

func TestEmptyInputReturnsEmpty(t *testing.T) {
	root := findRepoRoot(t)
	mockPath := filepath.Join(root, "third_party", "whisper.cpp",
		"models", "for-tests-ggml-tiny.bin")

	w, err := New(Config{ModelPath: mockPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	out, err := w.Transcribe(nil)
	if err != nil {
		t.Errorf("err on nil input: %v", err)
	}
	if out != "" {
		t.Errorf("out=%q, want empty", out)
	}
}

func TestMissingModelErrors(t *testing.T) {
	_, err := New(Config{ModelPath: "/does/not/exist.bin"})
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestEmptyPathErrors(t *testing.T) {
	_, err := New(Config{ModelPath: ""})
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}
