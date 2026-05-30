// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/audio"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
)

// ttsLead is how far ahead of real time we keep the device's playback queue
// while streaming TTS. Big enough to absorb encode/network jitter (and the
// Kokoro fetch gap between sentences within a turn) without an audible
// underrun; small enough that tts:stop lands close to true playback end so
// the mic doesn't re-open while the speaker is still talking. v1-specific:
// v2 replaces this real-time pacing with credit-based flow control.
const ttsLead = 400 * time.Millisecond

// v1Out implements deviceOut for Protocol v1: typed JSON control frames plus
// raw Opus binary frames. It owns the reply-scoped Speaking session — one
// tts:start … tts:stop pair even across many sentences — and the real-time
// frame pacing the AEC-less device requires (see docs/PROTOCOL_V1.md §5.2 and
// the SpeakPCM/SpeakEnd comments). Not safe for concurrent replies: SpeakBegin
// holds mu for the lifetime of the reply so two replies can't interleave their
// frames on the wire.
type v1Out struct {
	conn    *websocket.Conn
	encoder *audio.Encoder
	log     *slog.Logger

	mu         sync.Mutex // held SpeakBegin → SpeakEnd
	started    bool
	frameCount int
	start      time.Time
	frameDur   time.Duration
}

func newV1Out(conn *websocket.Conn, encoder *audio.Encoder, log *slog.Logger) *v1Out {
	frameMS := 0
	if encoder != nil {
		frameMS = encoder.FrameMS()
	}
	return &v1Out{
		conn:     conn,
		encoder:  encoder,
		log:      log,
		frameDur: time.Duration(frameMS) * time.Millisecond,
	}
}

// Transcript sends the ASR result (display-only on the device).
func (o *v1Out) Transcript(ctx context.Context, text string) error {
	return writeJSON(ctx, o.conn, protocol.STT{Type: "stt", Text: text})
}

// Display drives the avatar. v1 carries only the emotion tag; status is part
// of the implicit tts/listen lifecycle in v1, so it is dropped here.
func (o *v1Out) Display(ctx context.Context, emotion, _ string) error {
	return writeJSON(ctx, o.conn, protocol.LLM{Type: "llm", Emotion: emotion})
}

// SpeakBegin opens the reply's single Speaking session: grabs the wire lock
// and sends tts:start. The continuous frame clock starts here and spans every
// sentence in the reply so the lead buffer absorbs the inter-sentence Kokoro
// fetch gap.
func (o *v1Out) SpeakBegin(ctx context.Context) error {
	if o.encoder == nil {
		return errors.New("session: opus encoder not configured")
	}
	o.mu.Lock()
	if err := writeJSON(ctx, o.conn, protocol.TTS{Type: "tts", State: "start"}); err != nil {
		o.mu.Unlock()
		return fmt.Errorf("send tts:start: %w", err)
	}
	o.started = true
	o.frameCount = 0
	o.start = time.Now()
	return nil
}

// Caption sends one sentence as a tts:sentence_start chunk inside the open
// Speaking session. The device shows it as-is; v1 captions are per-sentence,
// not cumulative.
func (o *v1Out) Caption(ctx context.Context, segment string) error {
	return writeJSON(ctx, o.conn, protocol.TTS{Type: "tts", State: "sentence_start", Text: segment})
}

// SpeakPCM streams one sentence's PCM as Opus binary frames, paced at ~real
// time against the reply-wide clock and kept ttsLead ahead. Negative sleep
// durations (the initial prime burst, or catching up after a slow fetch) don't
// sleep. The frame clock is continuous across sentences (it is not reset
// between SpeakPCM calls), which is what lets the lead buffer hide the gap.
func (o *v1Out) SpeakPCM(ctx context.Context, pcm io.Reader) error {
	// Writes use a context detached from cancellation: a barge-in cancels ctx,
	// and a Write cancelled mid-flight makes coder/websocket tear down the whole
	// connection. Cancellation is instead observed between frames by the pacing
	// select below, so the stream stops cleanly and the socket survives.
	writeCtx := context.WithoutCancel(ctx)
	framer := audio.NewPCMFramer(pcm, o.encoder.SamplesPerFrame())
	for framer.Next() {
		opusBytes, err := o.encoder.Encode(framer.Frame())
		if err != nil {
			return fmt.Errorf("opus encode frame %d: %w", o.frameCount, err)
		}
		// Copy because the encoder reuses its scratch slice.
		out := append([]byte(nil), opusBytes...)
		if err := o.conn.Write(writeCtx, websocket.MessageBinary, out); err != nil {
			return fmt.Errorf("send opus frame %d: %w", o.frameCount, err)
		}
		o.frameCount++

		target := o.start.Add(time.Duration(o.frameCount)*o.frameDur - ttsLead)
		if d := time.Until(target); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := framer.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("pcm read: %w", err)
	}
	return nil
}

// SpeakEnd closes the Speaking session: drains the lead buffer so tts:stop
// lands at true end-of-audio (not ~ttsLead early, which would re-open the mic
// while the last word is still playing and clip it), sends tts:stop, and
// releases the wire lock. No-op if no reply was open, so it is safe to defer
// unconditionally.
func (o *v1Out) SpeakEnd(ctx context.Context) error {
	if !o.started {
		return nil
	}
	o.started = false
	defer o.mu.Unlock()

	target := o.start.Add(time.Duration(o.frameCount) * o.frameDur)
	if d := time.Until(target); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			_ = writeJSON(context.WithoutCancel(ctx), o.conn, protocol.TTS{Type: "tts", State: "stop"})
			return ctx.Err()
		}
	}
	if err := writeJSON(ctx, o.conn, protocol.TTS{Type: "tts", State: "stop"}); err != nil {
		return fmt.Errorf("send tts:stop: %w", err)
	}
	o.log.Info("speak end", "frames", o.frameCount, "audio_ms", o.frameCount*o.encoder.FrameMS())
	return nil
}

// Close sends a normal-closure frame. The device's OnAudioChannelClosed
// callback transitions it back to idle; the firmware reconnects lazily on the
// next wake word.
func (o *v1Out) Close(_ context.Context, reason string) error {
	return o.conn.Close(websocket.StatusNormalClosure, reason)
}

// v1Decoder maps inbound v1 wire frames to normalized inEvents. It collapses
// v1's overloaded "listen" type into the start/stop/wake split that v2 sends
// natively, so the read loop dispatches the same events regardless of protocol.
type v1Decoder struct{}

func (v1Decoder) Decode(data []byte) (inEvent, error) {
	msg, err := protocol.Decode(data)
	if err != nil {
		return nil, err
	}
	switch m := msg.(type) {
	case protocol.ClientHello:
		return evDupHello{}, nil
	case protocol.Listen:
		switch m.State {
		case "start":
			return evListenStart{Mode: m.Mode}, nil
		case "stop":
			return evListenStop{}, nil
		case "detect":
			return evWake{Phrase: m.Text}, nil
		default:
			return evUnknown{Type: "listen:" + m.State}, nil
		}
	case protocol.Abort:
		return evAbort{Reason: m.Reason}, nil
	case protocol.MCP:
		return evMCP{Payload: m.Payload}, nil
	case protocol.Event:
		return evTelemetry{Name: m.Name, Data: m.Data}, nil
	case protocol.Unknown:
		return evUnknown{Type: m.Type}, nil
	default:
		return evUnknown{Type: fmt.Sprintf("%T", m)}, nil
	}
}
