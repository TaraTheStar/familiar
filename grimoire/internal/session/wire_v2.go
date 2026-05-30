// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/audio"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
)

// defaultAudioCredit is the server→device audio credit advertised in the v2
// hello when none is configured (§5.1): 40 frames ≈ 2.4s at 60ms, matching the
// ESP32's fixed 40-packet decoder buffer.
const defaultAudioCredit = 40

// v2Out implements deviceOut (and creditSink) for Protocol v2. Where v1 paces
// audio against a wall clock, v2 gates each binary frame on credit-based flow
// control (§5): the device refills the budget via audio_credit as its decoder
// queue drains, so the fixed ESP32 buffer can never overrun. Like v1Out it
// holds mu for the lifetime of a reply (SpeakBegin → SpeakEnd) so two replies
// can't interleave frames, and it owns the per-utterance correlation id the v2
// wire threads through audio_begin/caption/audio_end.
//
// Captions are cumulative (§4.4): the loop hands Caption one sentence segment
// at a time, and v2Out accumulates them, emitting the full caption-so-far each
// time so the device displays text verbatim with no accumulation logic. The
// terminal caption (Final=true) is sent at SpeakEnd.
type v2Out struct {
	conn    *websocket.Conn
	encoder *audio.Encoder
	log     *slog.Logger

	mu        sync.Mutex // held SpeakBegin → SpeakEnd
	started   bool
	utterance int      // correlation id of the open utterance
	segs      []string // caption segments accumulated this reply

	// credits is the server's send budget for server→device audio frames.
	// Decremented per frame in SpeakPCM, replenished by AddCredit from the
	// read loop. wake signals a waiting SpeakPCM that fresh credit arrived.
	credits atomic.Int64
	wake    chan struct{}

	nextUtterance atomic.Int64 // monotonic utterance_id source, starts at 1
}

func newV2Out(conn *websocket.Conn, encoder *audio.Encoder, log *slog.Logger, initialCredit int) *v2Out {
	o := &v2Out{
		conn:    conn,
		encoder: encoder,
		log:     log,
		wake:    make(chan struct{}, 1),
	}
	o.credits.Store(int64(initialCredit))
	return o
}

// Transcript sends the ASR result (display-only). Final is always true until a
// streaming ASR exists (§11 Q6).
func (o *v2Out) Transcript(ctx context.Context, text string) error {
	return writeJSON(ctx, o.conn, protov2.Transcript{Type: "transcript", Text: text, Final: true})
}

// Display drives the avatar. Unlike v1, v2 carries both emotion and status.
func (o *v2Out) Display(ctx context.Context, emotion, status string) error {
	return writeJSON(ctx, o.conn, protov2.Display{Type: "display", Emotion: emotion, Status: status})
}

// SpeakBegin opens the reply's single audio stream: grabs the wire lock, mints
// a fresh utterance_id, and sends audio_begin. Caption accumulation resets for
// the new utterance.
func (o *v2Out) SpeakBegin(ctx context.Context) error {
	if o.encoder == nil {
		return errors.New("session: opus encoder not configured")
	}
	o.mu.Lock()
	o.utterance = int(o.nextUtterance.Add(1))
	o.segs = o.segs[:0]
	if err := writeJSON(ctx, o.conn, protov2.AudioBegin{Type: "audio_begin", UtteranceID: o.utterance}); err != nil {
		o.mu.Unlock()
		return fmt.Errorf("send audio_begin: %w", err)
	}
	o.started = true
	return nil
}

// Caption accumulates one sentence segment and emits the cumulative caption so
// far (Final=false). The device displays Text verbatim. The terminal Final=true
// caption is sent by SpeakEnd.
func (o *v2Out) Caption(ctx context.Context, segment string) error {
	o.segs = append(o.segs, segment)
	return writeJSON(ctx, o.conn, protov2.Caption{
		Type:        "caption",
		UtteranceID: o.utterance,
		Text:        strings.Join(o.segs, " "),
		Final:       false,
	})
}

// SpeakPCM streams one sentence's PCM as Opus binary frames, each gated on a
// credit. When the budget hits zero it blocks until AddCredit (driven by the
// device's audio_credit messages) replenishes it, so the device's fixed buffer
// never overruns (§5.4). No wall-clock pacing — credits are the backpressure.
func (o *v2Out) SpeakPCM(ctx context.Context, pcm io.Reader) error {
	// Frame writes use a context detached from cancellation: a barge-in cancels
	// ctx, and a Write cancelled mid-flight makes coder/websocket tear down the
	// whole connection (a partial frame would corrupt the stream). Instead we
	// observe cancellation at frame boundaries (the ctx check + acquireCredit)
	// so the stream stops cleanly within one frame and the socket survives.
	writeCtx := context.WithoutCancel(ctx)
	framer := audio.NewPCMFramer(pcm, o.encoder.SamplesPerFrame())
	for framer.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := o.acquireCredit(ctx); err != nil {
			return err
		}
		opusBytes, err := o.encoder.Encode(framer.Frame())
		if err != nil {
			return fmt.Errorf("opus encode: %w", err)
		}
		// Copy because the encoder reuses its scratch slice.
		out := append([]byte(nil), opusBytes...)
		if err := o.conn.Write(writeCtx, websocket.MessageBinary, out); err != nil {
			return fmt.Errorf("send opus frame: %w", err)
		}
	}
	if err := framer.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("pcm read: %w", err)
	}
	return nil
}

// acquireCredit consumes one audio credit, blocking until one is available or
// the context is cancelled.
func (o *v2Out) acquireCredit(ctx context.Context) error {
	for {
		if cur := o.credits.Load(); cur > 0 {
			if o.credits.CompareAndSwap(cur, cur-1) {
				return nil
			}
			continue // lost the race; re-read and retry
		}
		select {
		case <-o.wake:
			// Fresh credit may have arrived; loop re-checks.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// AddCredit replenishes the audio send budget (creditSink). Called from the
// read loop on each device audio_credit; wakes any SpeakPCM blocked on credit.
func (o *v2Out) AddCredit(frames int) {
	if frames <= 0 {
		return
	}
	o.credits.Add(int64(frames))
	select {
	case o.wake <- struct{}{}:
	default: // a wake is already pending; the waiter will re-check the counter
	}
}

// SpeakEnd closes the audio stream and releases the wire lock. No-op if no
// reply was open, so it is safe to defer unconditionally. There is no pacing
// drain (unlike v1) — the device leaves the speaking state when its own buffer
// empties.
//
// A cancelled context means the turn was interrupted (barge-in / session
// teardown): the stream is aborted with audio_cancel so the device flushes its
// decoder queue (PROTOCOL_V2 §4.4/§8), rather than ended normally. Otherwise
// the terminal cumulative caption (Final=true) and audio_end are sent. Either
// way the closing frames detach from ctx cancellation so they still reach a
// live socket.
func (o *v2Out) SpeakEnd(ctx context.Context) error {
	if !o.started {
		return nil
	}
	o.started = false
	defer o.mu.Unlock()

	sendCtx := context.WithoutCancel(ctx)
	if ctx.Err() != nil {
		o.log.Info("speak cancelled (barge-in)", "utterance_id", o.utterance)
		if err := writeJSON(sendCtx, o.conn, protov2.AudioCancel{Type: "audio_cancel", UtteranceID: o.utterance}); err != nil {
			return fmt.Errorf("send audio_cancel: %w", err)
		}
		return nil
	}

	if len(o.segs) > 0 {
		if err := writeJSON(sendCtx, o.conn, protov2.Caption{
			Type:        "caption",
			UtteranceID: o.utterance,
			Text:        strings.Join(o.segs, " "),
			Final:       true,
		}); err != nil {
			return fmt.Errorf("send final caption: %w", err)
		}
	}
	if err := writeJSON(sendCtx, o.conn, protov2.AudioEnd{Type: "audio_end", UtteranceID: o.utterance}); err != nil {
		return fmt.Errorf("send audio_end: %w", err)
	}
	o.log.Info("speak end", "utterance_id", o.utterance)
	return nil
}

// SendError emits a first-class v2 error (§4.9). Best-effort: a failed send is
// logged-by-omission since the session is already on an error path.
func (o *v2Out) SendError(ctx context.Context, code, message string, refID int) {
	_ = writeJSON(ctx, o.conn, protov2.Error{Type: "error", Code: code, Message: message, RefID: refID})
}

// Close sends an advisory goodbye (§4.10) naming the reason, then the WS close
// frame. The close frame does the real work; the device returns to idle on
// OnAudioChannelClosed and reconnects lazily on the next wake word.
func (o *v2Out) Close(ctx context.Context, reason string) error {
	// goodbye is best-effort and advisory; ignore its error and always close.
	_ = writeJSON(ctx, o.conn, protov2.Goodbye{Type: "goodbye", Reason: goodbyeReason(reason)})
	return o.conn.Close(websocket.StatusNormalClosure, reason)
}

// goodbyeReason maps the loop's internal close reason to a v2 goodbye reason
// (§4.10). The loop closes with "goodbye" on an exit phrase, which is a user
// farewell; anything else is reported verbatim and the recipient tolerates
// unknown reasons.
func goodbyeReason(reason string) string {
	if reason == "goodbye" {
		return "user_farewell"
	}
	return reason
}

// v2Decoder maps inbound v2 wire frames to normalized inEvents. v2 sends the
// listen/wake split natively, so unlike v1Decoder there is no overloaded-type
// collapsing to do — the mapping is one-to-one.
type v2Decoder struct{}

func (v2Decoder) Decode(data []byte) (inEvent, error) {
	msg, err := protov2.Decode(data)
	if err != nil {
		return nil, err
	}
	switch m := msg.(type) {
	case protov2.ClientHello:
		return evDupHello{}, nil
	case protov2.ListenStart:
		return evListenStart{Mode: m.Mode}, nil
	case protov2.ListenStop:
		return evListenStop{}, nil
	case protov2.Wake:
		return evWake{Phrase: m.Phrase, Score: m.Score}, nil
	case protov2.Abort:
		return evAbort{Reason: m.Reason}, nil
	case protov2.Telemetry:
		return evTelemetry{Name: m.Event, Data: m.Data}, nil
	case protov2.AudioCredit:
		return evAudioCredit{Frames: m.Frames}, nil
	case protov2.Goodbye:
		return evGoodbye{Reason: m.Reason}, nil
	case protov2.Error:
		return evError{Code: m.Code, Message: m.Message}, nil
	case protov2.ToolList, protov2.ToolCall:
		// First-class tool responses: hand the whole frame to the v2 tool port,
		// which correlates it by id (see tools_v2.go). Unmatched ids (e.g.
		// device-initiated requests we don't serve) are dropped there.
		return evToolResponse{Raw: append([]byte(nil), data...)}, nil
	case protov2.Unknown:
		return evUnknown{Type: m.Type}, nil
	default:
		return evUnknown{Type: fmt.Sprintf("%T", m)}, nil
	}
}
