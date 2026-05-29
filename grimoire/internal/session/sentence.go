// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import "strings"

// sentenceBuffer accumulates LLM stream deltas and yields complete
// sentences as they become available. Goal: TTS can start speaking the
// first sentence while the LLM is still generating later ones, hiding
// latency.
//
// Sentence boundary: `.`, `!`, or `?` followed by whitespace or end of
// string. Newlines also count as boundaries (handles markdown-ish output).
//
// Known imperfect cases that we accept:
//   - "Mr. Smith" splits on "Mr." — fine for TTS (two short utterances)
//   - Numbers like "1.5" don't split (the dot isn't followed by space)
//   - URLs / emails split on every dot — degraded but rare in chatty TTS
//
// If these bite, upgrade to a sentence tokenizer (e.g. punkt).
type sentenceBuffer struct {
	buf strings.Builder
}

// Add appends delta. Returns any complete sentences that have appeared
// since the last call, in order. Partial trailing text stays buffered
// for the next call (or for Flush()).
func (s *sentenceBuffer) Add(delta string) []string {
	if delta == "" {
		return nil
	}
	s.buf.WriteString(delta)
	out, rest := splitSentences(s.buf.String())
	s.buf.Reset()
	s.buf.WriteString(rest)
	return out
}

// Flush returns the buffered text as a final (possibly punctuation-less)
// sentence, then clears the buffer. Call after the LLM stream ends.
func (s *sentenceBuffer) Flush() string {
	out := strings.TrimSpace(s.buf.String())
	s.buf.Reset()
	return out
}

// splitSentences finds sentence boundaries in text. A boundary is one of
// `.`, `!`, `?` followed immediately by whitespace. Returns the complete
// sentences (trimmed) and the unmatched tail.
func splitSentences(text string) (complete []string, remainder string) {
	start := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '.' && c != '!' && c != '?' && c != '\n' {
			continue
		}
		// Newlines: treat as a boundary regardless of what came before.
		if c == '\n' {
			seg := strings.TrimSpace(text[start : i+1])
			if seg != "" {
				complete = append(complete, seg)
			}
			start = i + 1
			continue
		}
		// `.`/`!`/`?`: boundary only if next byte is whitespace.
		if i+1 >= len(text) {
			continue // could be a complete sentence at EOF; leave to Flush
		}
		next := text[i+1]
		if next == ' ' || next == '\n' || next == '\t' {
			seg := strings.TrimSpace(text[start : i+1])
			if seg != "" {
				complete = append(complete, seg)
			}
			start = i + 1
		}
	}
	return complete, text[start:]
}
