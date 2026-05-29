// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"reflect"
	"testing"
)

func TestSentenceBufferStreamsCompleteSentences(t *testing.T) {
	var b sentenceBuffer

	// Stream a multi-sentence reply in arbitrary chunks (matching LLM
	// token deltas).
	if got := b.Add("Hi there"); len(got) != 0 {
		t.Errorf("partial yielded: %v", got)
	}
	if got := b.Add("! How can I"); !reflect.DeepEqual(got, []string{"Hi there!"}) {
		t.Errorf("first sentence: %v", got)
	}
	if got := b.Add(" help you today"); len(got) != 0 {
		t.Errorf("partial yielded: %v", got)
	}
	if got := b.Add("? "); !reflect.DeepEqual(got, []string{"How can I help you today?"}) {
		t.Errorf("second sentence: %v", got)
	}
	if got := b.Flush(); got != "" {
		t.Errorf("expected empty flush, got %q", got)
	}
}

func TestSentenceBufferFlushesPartial(t *testing.T) {
	var b sentenceBuffer
	b.Add("Just one fragment without punctuation")
	if got := b.Flush(); got != "Just one fragment without punctuation" {
		t.Errorf("flush: %q", got)
	}
}

func TestSentenceBufferHandlesNewlines(t *testing.T) {
	var b sentenceBuffer
	// Markdown-ish output: line breaks act as boundaries.
	got := b.Add("First line\nSecond line\nThird")
	want := []string{"First line", "Second line"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if rest := b.Flush(); rest != "Third" {
		t.Errorf("flush: %q", rest)
	}
}

func TestSentenceBufferKeepsDecimalsTogether(t *testing.T) {
	var b sentenceBuffer
	// "1.5" — the dot isn't followed by whitespace, so no split.
	got := b.Add("Version 1.5 is out.")
	want := []string{"Version 1.5 is out."}
	// Without a trailing space the final `.` doesn't trigger a boundary
	// during Add — but Flush should yield it.
	if len(got) != 0 {
		// Either behavior is acceptable; document what we got.
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
		return
	}
	if rest := b.Flush(); rest != "Version 1.5 is out." {
		t.Errorf("flush: %q", rest)
	}
}

func TestSentenceBufferSplitsMrButThatsOK(t *testing.T) {
	// Accepted imperfection: titles like "Mr." split. Test documents it
	// so the next person doesn't think it's a bug.
	var b sentenceBuffer
	got := b.Add("Mr. Smith arrived.")
	want := []string{"Mr.", "Smith arrived."}
	if !reflect.DeepEqual(got, want) && b.Flush() != "Smith arrived." {
		// Either we split twice (yielding both) OR once + flush gives the rest.
		t.Errorf("unexpected: streamed=%v, flush=%q", got, b.Flush())
	}
}

func TestSentenceBufferIgnoresEmptyDelta(t *testing.T) {
	var b sentenceBuffer
	if got := b.Add(""); got != nil {
		t.Errorf("empty Add should be no-op, got %v", got)
	}
}
