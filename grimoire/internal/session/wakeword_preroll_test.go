// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// recordingASR captures the byte length of the mic PCM handed to it, so a test
// can assert how much audio the server buffered for the turn.
type recordingASR struct {
	mu      sync.Mutex
	lastLen int
	out     string
}

func (r *recordingASR) Transcribe(pcm []int16) (string, error) {
	r.mu.Lock()
	r.lastLen = len(pcm) * 2 // report bytes, matching the wire frame accounting
	r.mu.Unlock()
	return r.out, nil
}

func (r *recordingASR) got() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastLen
}

// trivialContentLLM streams one content completion — enough to drive a turn to
// completion without tools.
func trivialContentLLM(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(s string) {
			fmt.Fprintf(w, "data: %s\n\n", s)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		write(`{"choices":[{"delta":{"content":"Okay."}}]}`)
		write(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		write("[DONE]")
	}))
}

// TestWakeWordPreroll proves PROTOCOL_V2 §4.2 wake_word_audio pre-roll: when the
// device advertises the feature, a `wake` from idle opens the turn's audio
// window so binary frames sent BEFORE listen_start are buffered as turn audio;
// without the feature those frames are dropped (window opens at listen_start).
//
// Each fake frame is 60ms @ 16kHz mono → 960 samples → 1920 bytes decoded. The
// scenario sends 2 pre-roll frames + 2 live frames, so the ASR sees 4 frames
// with the feature and only the 2 live frames without it. Manual listen mode is
// used so the turn fires deterministically on listen_stop (no server VAD).
func TestWakeWordPreroll(t *testing.T) {
	const frameBytes = 960 * 2

	withFeature := runPrerollScenario(t, true)
	if withFeature != 4*frameBytes {
		t.Errorf("with wake_word_audio: ASR saw %d bytes, want %d (2 pre-roll + 2 live)",
			withFeature, 4*frameBytes)
	}

	without := runPrerollScenario(t, false)
	if without != 2*frameBytes {
		t.Errorf("without feature: ASR saw %d bytes, want %d (pre-roll dropped, 2 live only)",
			without, 2*frameBytes)
	}
}

// runPrerollScenario drives one wake → pre-roll → listen_start → live →
// listen_stop turn and returns the mic PCM byte count the ASR received. The wire
// sequence is byte-identical regardless of `advertise`; only the hello's feature
// list differs, isolating the pre-roll behavior to the advertised feature.
func runPrerollScenario(t *testing.T, advertise bool) int {
	t.Helper()
	asr := &recordingASR{out: "hello there"}
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
		LLM:              &llm.Client{BaseURL: llmSrv.URL, Model: "test"},
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Protocol-Version": []string{"2"}},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	features := []string{"tools"}
	if advertise {
		features = append(features, "wake_word_audio")
	}
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ClientHello{
		Type: "hello", ID: 1,
		Client: protov2.ClientInfo{Name: "stack-chan"},
		Audio: protov2.AudioConfig{
			In:  protov2.AudioStream{Codec: "opus", Rate: 16000, Channels: 1, FrameMS: 60},
			Out: protov2.AudioStream{Codec: "opus", Rate: 24000, Channels: 1, FrameMS: 60},
		},
		Features: features,
	}))
	mustReadText(t, ctx, conn) // server hello

	// Answer tool discovery with an empty catalog so initTools completes fast.
	tlReq := readToolFrame(t, ctx, conn)
	tl, ok := tlReq.(protov2.ToolList)
	if !ok {
		t.Fatalf("expected tool_list discovery, got %T", tlReq)
	}
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ToolList{
		Type: "tool_list", ID: tl.ID, Result: &protov2.ToolListResult{},
	}))

	// wake → 2 pre-roll frames → listen_start(manual) → 2 live frames → stop.
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.Wake{Type: "wake", Phrase: "hi_stackchan", Score: 0.9}))
	sendFakeAudioFrame(t, ctx, conn) // pre-roll 1 (buffered iff feature advertised)
	sendFakeAudioFrame(t, ctx, conn) // pre-roll 2
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStart{Type: "listen_start", Mode: "manual"}))
	sendFakeAudioFrame(t, ctx, conn) // live 1 (always buffered)
	sendFakeAudioFrame(t, ctx, conn) // live 2
	mustWriteText(t, ctx, conn, mustJSON(t, protov2.ListenStop{Type: "listen_stop"}))

	// The turn runs on a goroutine; handleTurn calls ASR first, then emits the
	// transcript. Seeing transcript guarantees ASR ran and lastLen is set.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		mt, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if mt != websocket.MessageText {
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if head.Type == "transcript" {
			return asr.got()
		}
	}
	t.Fatal("never saw transcript frame (turn did not run)")
	return 0
}
