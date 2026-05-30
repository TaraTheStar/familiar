// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// updateGolden regenerates the testdata/golden/*.txt fixtures instead of
// comparing against them. Run: GOWORK=off go test ./internal/session -run Golden -update
var updateGolden = flag.Bool("update", false, "update golden wire-frame fixtures")

// These golden tests pin the EXACT server→device wire output of the v1
// protocol for representative session flows. They exist to make the
// upcoming protocol-seam extraction provably non-regressive: the v1 frames
// the device sees must not change. Each fixture is the ordered sequence of
// outbound frames, one per line:
//
//   - text frames    → the verbatim JSON, with the random session_id
//                       normalized to "SID" (see normalizeFrame)
//   - binary frames  → "<binary opus N bytes>" (Opus bytes themselves are
//                       the audio package's concern; we pin count + position)
//   - graceful close → "<close CODE reason>"
//
// If a change legitimately alters the v1 wire output, regenerate with
// -update and review the diff — an unintended change is exactly what these
// tests are meant to catch.

// recordSession drives one client flow against a v1 session and returns the
// ordered, normalized outbound frames. `flow` is the client side of the
// exchange (it has already had ClientHello sent for it); it should send the
// device→server messages that provoke the output under test. Reading stops
// when the server goes idle for idleStop or closes the socket.
func recordSession(t *testing.T, cfg Config, flow func(t *testing.T, ctx context.Context, conn *websocket.Conn)) []string {
	t.Helper()

	srv := httptest.NewServer(Handler(cfg))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	// Every v1 session opens with the client hello; send it for the flow so
	// each fixture starts from the ServerHello reply.
	mustWriteText(t, ctx, conn, mustJSON(t, protocol.ClientHello{
		Type: "hello", Version: 1, Transport: "websocket",
		AudioParams: &protocol.AudioParams{Format: "opus", SampleRate: 16000, Channels: 1, FrameDuration: 60},
	}))

	// Run the client side concurrently with the read collector so a flow can
	// send mid-stream messages (e.g. listen:stop) while we drain output.
	flowDone := make(chan struct{})
	go func() {
		defer close(flowDone)
		flow(t, ctx, conn)
	}()

	frames := collectFrames(t, ctx, conn, 1500*time.Millisecond)
	<-flowDone
	return frames
}

// recordSessionV2 is recordSession's Protocol v2 sibling: it upgrades with the
// Protocol-Version: 2 header so the server binds the v2 seam, sends a v2 client
// hello, then records the v2 wire output of the flow. v2 carries no session_id
// and no MCP frames, so collectFrames needs no v2-specific normalization.
func recordSessionV2(t *testing.T, cfg Config, flow func(t *testing.T, ctx context.Context, conn *websocket.Conn)) []string {
	t.Helper()

	srv := httptest.NewServer(Handler(cfg))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Protocol-Version": []string{"2"}},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ClientHello{
		Type: "hello", ID: 1,
		Client: protov2.ClientInfo{Name: "stack-chan", Version: "2.0.0"},
		Audio: protov2.AudioConfig{
			In:  protov2.AudioStream{Codec: "opus", Rate: 16000, Channels: 1, FrameMS: 60},
			Out: protov2.AudioStream{Codec: "opus", Rate: 24000, Channels: 1, FrameMS: 60},
		},
		Features: []string{"tools"},
	}))

	flowDone := make(chan struct{})
	go func() {
		defer close(flowDone)
		flow(t, ctx, conn)
	}()

	frames := collectFrames(t, ctx, conn, 1500*time.Millisecond)
	<-flowDone
	return frames
}

// collectFrames reads outbound frames until the server is silent for idleStop
// or closes the socket. Normalizes each frame to its golden representation.
func collectFrames(t *testing.T, ctx context.Context, conn *websocket.Conn, idleStop time.Duration) []string {
	t.Helper()
	var out []string
	for {
		readCtx, cancel := context.WithTimeout(ctx, idleStop)
		mt, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			if cs := websocket.CloseStatus(err); cs != -1 {
				out = append(out, fmt.Sprintf("<close %d %s>", cs, closeReason(err)))
			}
			// Idle timeout (DeadlineExceeded) or close: flow is done.
			return out
		}
		switch mt {
		case websocket.MessageBinary:
			out = append(out, fmt.Sprintf("<binary opus %d bytes>", len(data)))
		case websocket.MessageText:
			// Tool-registry frames (v1 mcp envelopes, v2 tool_list/tool_call)
			// are written from the discovery goroutine, so their position
			// relative to the turn frames is timing-dependent. They belong to
			// the tool port (pinned by its own tests) and are untouched by the
			// voice-loop wire output these fixtures guard, so drop them to keep
			// the sequence deterministic and on-topic.
			if isToolFrame(data) {
				continue
			}
			out = append(out, normalizeFrame(string(data)))
		}
	}
}

// sessionIDRE matches the random hex session_id the server mints per
// connection so it can be normalized to a stable token in fixtures.
var sessionIDRE = regexp.MustCompile(`"session_id":"[0-9a-f]+"`)

// unixMsRE matches the v2 hello's wall-clock timestamp (hello.time.unix_ms) so
// it can be pinned to a stable token; the surrounding shape still gets pinned.
var unixMsRE = regexp.MustCompile(`"unix_ms":\d+`)

func normalizeFrame(s string) string {
	s = sessionIDRE.ReplaceAllString(s, `"session_id":"SID"`)
	return unixMsRE.ReplaceAllString(s, `"unix_ms":0`)
}

// isToolFrame reports whether a text frame belongs to the tool registry: a v1
// MCP envelope or a v2 first-class tool_list/tool_call. These are filtered from
// the voice-loop golden fixtures (see collectFrames).
func isToolFrame(data []byte) bool {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return false
	}
	switch head.Type {
	case "mcp", "tool_list", "tool_call":
		return true
	default:
		return false
	}
}

// closeReason extracts the human reason text from a websocket close error,
// or "" if none. coder/websocket formats it as `status = StatusX and reason = "..."`.
func closeReason(err error) string {
	msg := err.Error()
	const marker = `reason = "`
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// assertGolden compares got against testdata/golden/<name>.txt, or rewrites
// the fixture when -update is set.
func assertGolden(t *testing.T, name string, got []string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".txt")
	body := strings.Join(got, "\n") + "\n"

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated golden %s (%d frames)", path, len(got))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to create)", path, err)
	}
	if string(want) != body {
		t.Errorf("wire output drift for %q.\n--- got ---\n%s\n--- want ---\n%s\nRun with -update if this change is intended.", name, body, want)
	}
}

// --- fakes --------------------------------------------------------------

// constantPCMKokoro returns a fake Kokoro that emits a fixed number of PCM
// samples per request, so binary-frame counts are deterministic.
func constantPCMKokoro(t *testing.T, samples int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(make([]byte, samples*2)) // s16 = 2 bytes/sample
	}))
}

// sentenceStreamLLM returns a fake OpenAI-compatible endpoint that streams
// the given content deltas then [DONE]. No tool calls.
func sentenceStreamLLM(t *testing.T, deltas ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, d := range deltas {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", d)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// --- the golden flows ---------------------------------------------------

// TestGoldenHandshake pins the ServerHello reply shape.
func TestGoldenHandshake(t *testing.T) {
	got := recordSession(t, Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
	}, func(t *testing.T, ctx context.Context, conn *websocket.Conn) {
		// Hello already sent by recordSession; nothing more to provoke.
	})
	assertGolden(t, "handshake", got)
}

// TestGoldenTurnMultiSentence pins the full server→device frame sequence for
// a normal (non-exit) multi-sentence reply with no tool calls:
//
//	hello → stt → llm:thinking → llm:happy → tts:start →
//	  (sentence_start + opus frames)×3 → tts:stop → llm:neutral
func TestGoldenTurnMultiSentence(t *testing.T) {
	kokoro := constantPCMKokoro(t, 2880) // 120ms @24kHz = exactly 2 frames/sentence
	defer kokoro.Close()
	llmServer := sentenceStreamLLM(t,
		"Sentence", " one. ", "Sentence two", "! ", "And a final ", "fragment")
	defer llmServer.Close()

	got := recordSession(t, Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		MCPInitTimeout:   200 * time.Millisecond,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
		ASR:              &fakeASR{out: "what time is it"},
		LLM:              &llm.Client{BaseURL: llmServer.URL, Model: "test-model"},
		SystemPrompt:     "You are a test robot.",
	}, func(t *testing.T, ctx context.Context, conn *websocket.Conn) {
		mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "start", Mode: "auto"}))
		sendFakeAudioFrame(t, ctx, conn)
		mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "stop"}))
	})
	assertGolden(t, "turn_multi_sentence", got)
}

// TestGoldenTurnExitPhrase pins that an exit-phrase turn ends with a graceful
// close after the reply: the device returns to idle on WS close (no
// system/reboot). Locks the v1 farewell behavior.
func TestGoldenTurnExitPhrase(t *testing.T) {
	kokoro := constantPCMKokoro(t, 1440) // 60ms @24kHz = 1 frame
	defer kokoro.Close()
	llmServer := sentenceStreamLLM(t, "Goodbye!")
	defer llmServer.Close()

	got := recordSession(t, Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		MCPInitTimeout:   200 * time.Millisecond,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
		ASR:              &fakeASR{out: "goodbye"},
		LLM:              &llm.Client{BaseURL: llmServer.URL, Model: "test-model"},
	}, func(t *testing.T, ctx context.Context, conn *websocket.Conn) {
		mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "start", Mode: "auto"}))
		sendFakeAudioFrame(t, ctx, conn)
		mustWriteText(t, ctx, conn, mustJSON(t, protocol.Listen{Type: "listen", State: "stop"}))
	})
	assertGolden(t, "turn_exit_phrase", got)
}

// --- v2 golden flows ----------------------------------------------------
//
// The v2 fixtures pin the Protocol v2 wire output for the same flows as the v1
// fixtures above, driven through a v2 session (selected by the Protocol-Version
// header). Together with the seam-contract tests (seam_test.go) they prove the
// migration story: identical loop choreography, two faithful renderings.

// TestGoldenV2Handshake pins the v2 ServerHello: no session_id, echoed request
// id, negotiated audio in/out, and the initial audio credit.
func TestGoldenV2Handshake(t *testing.T) {
	got := recordSessionV2(t, Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
	}, func(t *testing.T, ctx context.Context, conn *websocket.Conn) {
		// Hello already sent by recordSessionV2; nothing more to provoke.
	})
	assertGolden(t, "v2_handshake", got)
}

// TestGoldenV2TurnMultiSentence pins the full v2 frame sequence for a
// multi-sentence reply: transcript → display(thinking) → display(speaking) →
// audio_begin → (cumulative caption + opus frames)×3 → final caption →
// audio_end → display(listening). Captions are cumulative (§4.4) and the audio
// stream is one utterance_id, the v2 rendering of the one-Speaking-session
// invariant. The flow refills audio credit to exercise the credit path (§5).
func TestGoldenV2TurnMultiSentence(t *testing.T) {
	kokoro := constantPCMKokoro(t, 2880) // 120ms @24kHz = exactly 2 frames/sentence
	defer kokoro.Close()
	llmServer := sentenceStreamLLM(t,
		"Sentence", " one. ", "Sentence two", "! ", "And a final ", "fragment")
	defer llmServer.Close()

	got := recordSessionV2(t, Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		MCPInitTimeout:   200 * time.Millisecond,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
		ASR:              &fakeASR{out: "what time is it"},
		LLM:              &llm.Client{BaseURL: llmServer.URL, Model: "test-model"},
		SystemPrompt:     "You are a test robot.",
	}, func(t *testing.T, ctx context.Context, conn *websocket.Conn) {
		mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStart{Type: "listen_start", Mode: "auto"}))
		sendFakeAudioFrame(t, ctx, conn)
		mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStop{Type: "listen_stop"}))
		// Refill credit (the real device sends these as its buffer drains).
		mustWriteText(t, ctx, conn, mustJSON(t, protov2.AudioCredit{Type: "audio_credit", Frames: 40}))
	})
	assertGolden(t, "v2_turn_multi_sentence", got)
}
