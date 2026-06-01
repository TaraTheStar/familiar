// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// progressiveASR returns a transcript whose word count grows with the input
// length, so a mid-utterance partial is a strict prefix of the final result —
// exactly the streaming-ASR shape (PROTOCOL_V2 §4.3). It also counts how many
// times it was invoked, which tells us partial re-inference actually happened.
type progressiveASR struct{ calls atomic.Int64 }

func (p *progressiveASR) Transcribe(pcm []int16) (string, error) {
	p.calls.Add(1)
	// One "word" per ~5k samples; always at least one so a short pass still
	// yields usable text (filterASRArtifacts keeps letters).
	n := len(pcm)/5000 + 1
	words := make([]string, n)
	for i := range words {
		words[i] = "word"
	}
	return strings.Join(words, " "), nil
}

// TestStreamingASR proves PROTOCOL_V2 §4.3: with ASRStreaming on, the server
// emits one or more transcript{final:false} partials while the user is still
// speaking, followed by exactly one authoritative transcript{final:true}. The
// turn uses manual listen mode so it fires deterministically on listen_stop
// (no server VAD), and sends enough frames to cross the ~700ms partial debounce
// several times.
func TestStreamingASR(t *testing.T) {
	asr := &progressiveASR{}
	partials, finalText := runStreamingASRScenario(t, asr, true, 30)

	if len(partials) == 0 {
		t.Fatalf("streaming on: expected ≥1 transcript{final:false} partial, got none")
	}
	if finalText == "" {
		t.Fatalf("streaming on: never saw the authoritative transcript{final:true}")
	}
	// Each partial reflects a growing buffer, so it should be a prefix of (or
	// equal to) the final transcript — never longer.
	for i, p := range partials {
		if len(p) > len(finalText) {
			t.Errorf("partial %d %q is longer than final %q", i, p, finalText)
		}
	}
	if asr.calls.Load() < 2 {
		t.Errorf("expected ≥2 ASR passes (partials + final), got %d", asr.calls.Load())
	}
}

// TestStreamingASRDisabled is the control: with ASRStreaming off, the server
// emits no partials — only the single final transcript, as before this feature.
func TestStreamingASRDisabled(t *testing.T) {
	asr := &progressiveASR{}
	partials, finalText := runStreamingASRScenario(t, asr, false, 30)

	if len(partials) != 0 {
		t.Errorf("streaming off: expected no partials, got %v", partials)
	}
	if finalText == "" {
		t.Fatalf("streaming off: never saw the final transcript")
	}
}

// runStreamingASRScenario drives one manual-mode turn of `frames` mic frames and
// returns the text of every transcript{final:false} partial (in order) plus the
// text of the single transcript{final:true}. It reads transcript frames only up
// to and including the final one.
func runStreamingASRScenario(t *testing.T, asr ASR, streaming bool, frames int) (partials []string, finalText string) {
	t.Helper()
	llmSrv := trivialContentLLM(t)
	defer llmSrv.Close()
	kokoro := inlineKokoroStub(t)
	defer kokoro.Close()

	srv := httptest.NewServer(Handler(Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, Channels: 1, FrameDuration: 60},
		MicAudio:         protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60},
		HandshakeTimeout: 2 * time.Second,
		MCPInitTimeout:   2 * time.Second,
		Kokoro:           &tts.KokoroClient{BaseURL: kokoro.URL},
		ASR:              asr,
		ASRStreaming:     streaming,
		LLM:              &llm.Client{BaseURL: llmSrv.URL, Model: "test"},
	}))
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
	defer conn.Close(websocket.StatusNormalClosure, "")

	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ClientHello{
		Type: "hello", ID: 1,
		Client: protov2.ClientInfo{Name: "stack-chan"},
		Audio: protov2.AudioConfig{
			In:  protov2.AudioStream{Codec: "opus", Rate: 16000, Channels: 1, FrameMS: 60},
			Out: protov2.AudioStream{Codec: "opus", Rate: 24000, Channels: 1, FrameMS: 60},
		},
		Features: []string{"tools"},
	}))
	mustReadText(t, ctx, conn) // server hello

	// Empty tool catalog so initTools completes fast.
	tlReq := readToolFrame(t, ctx, conn)
	tl, ok := tlReq.(protov2.ToolList)
	if !ok {
		t.Fatalf("expected tool_list discovery, got %T", tlReq)
	}
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ToolList{
		Type: "tool_list", ID: tl.ID, Result: &protov2.ToolListResult{},
	}))

	// Manual listen window: stream `frames` mic frames, then stop. Each frame is
	// 60ms @16kHz = 1920 bytes, so 30 frames ≈ 57.6 KB crosses the ~22.4 KB
	// (700ms) partial debounce ~2-3 times.
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStart{Type: "listen_start", Mode: "manual"}))
	for i := 0; i < frames; i++ {
		sendFakeAudioFrame(t, ctx, conn)
	}
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStop{Type: "listen_stop"}))

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mt, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if mt != websocket.MessageText {
			continue
		}
		var tr protov2.Transcript
		if err := json.Unmarshal(data, &tr); err != nil {
			continue
		}
		if tr.Type != "transcript" {
			continue
		}
		if tr.Final {
			return partials, tr.Text
		}
		partials = append(partials, tr.Text)
	}
	t.Fatal("never saw the final transcript frame (turn did not run)")
	return nil, ""
}
