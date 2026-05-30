// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// The v1↔v2 adapter harness (PROTOCOL_V2 §13): drive the SAME scripted turn
// through a v1 session and a v2 session, reduce each protocol's wire output to
// a protocol-agnostic semantic summary, and assert the two are identical. The
// goldens pin each protocol's exact bytes; the seam-contract test pins the
// loop's call order; this proves the two renderings are semantically equal —
// the migration promise that a device sees the same conversation either way.

// semanticTurn is what a turn means, independent of framing: the transcript
// shown, the ordered emotion tags, how many sentences were captioned and their
// combined text, how many audio frames played, and whether the session closed.
type semanticTurn struct {
	transcript   string
	emotions     []string
	sentenceN    int
	completeText string
	audioFrames  int
	closed       bool
}

// parseV1Turn reduces v1 wire frames (stt / llm / tts) to a semanticTurn.
func parseV1Turn(frames []string) semanticTurn {
	var st semanticTurn
	var sentences []string
	for _, fr := range frames {
		if parseNonJSON(fr, &st) {
			continue
		}
		f := decodeFrame(fr)
		switch f.Type {
		case "stt":
			st.transcript = f.Text
		case "llm":
			st.emotions = append(st.emotions, f.Emotion)
		case "tts":
			if f.State == "sentence_start" {
				sentences = append(sentences, f.Text)
			}
		}
	}
	st.sentenceN = len(sentences)
	st.completeText = strings.Join(sentences, " ")
	return st
}

// parseV2Turn reduces v2 wire frames (transcript / display / caption) to a
// semanticTurn. Captions are cumulative: each non-final one is a sentence, and
// the final one carries the complete text.
func parseV2Turn(frames []string) semanticTurn {
	var st semanticTurn
	for _, fr := range frames {
		if parseNonJSON(fr, &st) {
			continue
		}
		f := decodeFrame(fr)
		switch f.Type {
		case "transcript":
			st.transcript = f.Text
		case "display":
			st.emotions = append(st.emotions, f.Emotion)
		case "caption":
			if f.Final {
				st.completeText = f.Text
			} else {
				st.sentenceN++
			}
		}
	}
	return st
}

type wireFrame struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Emotion string `json:"emotion"`
	State   string `json:"state"`
	Final   bool   `json:"final"`
}

func decodeFrame(fr string) wireFrame {
	var f wireFrame
	_ = json.Unmarshal([]byte(fr), &f)
	return f
}

// parseNonJSON handles the harness's non-JSON frame renderings (binary audio,
// graceful close), updating st and reporting whether fr was one of them.
func parseNonJSON(fr string, st *semanticTurn) bool {
	switch {
	case strings.HasPrefix(fr, "<binary"):
		st.audioFrames++
		return true
	case strings.HasPrefix(fr, "<close"):
		st.closed = true
		return true
	default:
		return false
	}
}

func TestV1V2Equivalence(t *testing.T) {
	cases := []struct {
		name       string
		transcript string
		deltas     []string
		kokoroPCM  int  // samples per Kokoro response
		wantClose  bool // exit phrase → graceful close
	}{
		{
			name:       "multi_sentence",
			transcript: "what time is it",
			deltas:     []string{"Sentence", " one. ", "Sentence two", "! ", "And a final ", "fragment"},
			kokoroPCM:  2880, // 2 frames/sentence
			wantClose:  false,
		},
		{
			name:       "exit_phrase",
			transcript: "goodbye",
			deltas:     []string{"Goodbye!"},
			kokoroPCM:  1440, // 1 frame
			wantClose:  true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kokoro := constantPCMKokoro(t, c.kokoroPCM)
			defer kokoro.Close()
			llmServer := sentenceStreamLLM(t, c.deltas...)
			defer llmServer.Close()

			cfg := Config{
				TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
				MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
				HandshakeTimeout: 2 * time.Second,
				MCPInitTimeout:   200 * time.Millisecond,
				Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
				ASR:              &fakeASR{out: c.transcript},
				LLM:              &llm.Client{BaseURL: llmServer.URL, Model: "test-model"},
			}

			v1 := parseV1Turn(recordSession(t, cfg, func(t *testing.T, ctx context.Context, conn *websocket.Conn) {
				mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "start", Mode: "auto"}))
				sendFakeAudioFrame(t, ctx, conn)
				mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "stop"}))
			}))

			v2 := parseV2Turn(recordSessionV2(t, cfg, func(t *testing.T, ctx context.Context, conn *websocket.Conn) {
				mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStart{Type: "listen_start", Mode: "auto"}))
				sendFakeAudioFrame(t, ctx, conn)
				mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStop{Type: "listen_stop"}))
			}))

			assertSemanticEqual(t, v1, v2, c.wantClose)
		})
	}
}

func assertSemanticEqual(t *testing.T, v1, v2 semanticTurn, wantClose bool) {
	t.Helper()
	if v1.transcript != v2.transcript {
		t.Errorf("transcript: v1=%q v2=%q", v1.transcript, v2.transcript)
	}
	if strings.Join(v1.emotions, ",") != strings.Join(v2.emotions, ",") {
		t.Errorf("emotion sequence: v1=%v v2=%v", v1.emotions, v2.emotions)
	}
	if v1.sentenceN != v2.sentenceN {
		t.Errorf("sentence count: v1=%d v2=%d", v1.sentenceN, v2.sentenceN)
	}
	if v1.completeText != v2.completeText {
		t.Errorf("complete spoken text:\n v1=%q\n v2=%q", v1.completeText, v2.completeText)
	}
	if v1.audioFrames != v2.audioFrames {
		t.Errorf("audio frame count: v1=%d v2=%d", v1.audioFrames, v2.audioFrames)
	}
	if v1.closed != v2.closed {
		t.Errorf("close behavior: v1=%v v2=%v", v1.closed, v2.closed)
	}
	if v1.closed != wantClose {
		t.Errorf("close behavior = %v, want %v", v1.closed, wantClose)
	}
	// Sanity: the turn actually did something (guards against both sides being
	// silently empty, which would pass the equality checks vacuously).
	if v1.transcript == "" || v1.sentenceN == 0 || v1.audioFrames == 0 {
		t.Errorf("degenerate turn: %+v", v1)
	}
}
