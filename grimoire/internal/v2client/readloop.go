// SPDX-License-Identifier: AGPL-3.0-or-later

package v2client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
)

// readLoop is the device's receive side: it decodes server frames, answers tool
// discovery and calls, returns audio credit, decodes TTS to PCM, and fires
// turnComplete when the turn resolves. It runs until the connection closes.
func (c *client) readLoop(ctx context.Context) {
	defer close(c.readerExited)
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			// A normal/away close, our own graceful close, or ctx cancellation
			// all end the loop quietly; anything else is worth surfacing.
			if !isExpectedClose(err) && ctx.Err() == nil {
				c.logf("read loop ended: %v", err)
			}
			c.markComplete()
			return
		}
		if typ == websocket.MessageBinary {
			c.onAudioFrame(ctx, data)
			continue
		}
		if err := c.onTextFrame(ctx, data); err != nil {
			c.logf("handling frame: %v", err)
		}
	}
}

// onAudioFrame decodes one TTS Opus frame to PCM, accounts a consumed credit,
// and triggers a barge-in once enough frames have played (if configured).
func (c *client) onAudioFrame(ctx context.Context, opusBytes []byte) {
	pcm, err := c.dec.Decode(opusBytes)
	if err != nil {
		c.logf("decode tts frame: %v", err)
		return
	}

	c.mu.Lock()
	c.res.AudioFrames++
	for _, s := range pcm {
		c.res.AudioPCM = append(c.res.AudioPCM, byte(s), byte(s>>8))
	}
	frames := c.res.AudioFrames
	c.credit++
	grant := 0
	if c.credit >= c.creditBatch() {
		grant = c.credit
		c.credit = 0
	}
	wantBarge := c.cfg.BargeAfterFrames > 0 && frames >= c.cfg.BargeAfterFrames && !c.bargeSent
	if wantBarge {
		c.bargeSent = true
	}
	c.mu.Unlock()

	// Grant credit back so the server's send budget never starves (§5.2).
	if grant > 0 {
		if err := c.writeJSON(ctx, protov2.AudioCredit{Type: "audio_credit", Frames: grant}); err != nil {
			c.logf("grant audio_credit: %v", err)
		}
	}
	if wantBarge {
		c.logf("barge-in after %d frames: sending abort", frames)
		if err := c.writeJSON(ctx, protov2.Abort{Type: "abort", Reason: "user_action"}); err != nil {
			c.logf("send abort: %v", err)
		}
	}
}

// onTextFrame dispatches one control frame.
func (c *client) onTextFrame(ctx context.Context, data []byte) error {
	msg, err := decodeServerFrame(data)
	if err != nil {
		return err
	}
	switch m := msg.(type) {
	case protov2.Transcript:
		c.mu.Lock()
		c.res.Transcript = m.Text
		c.mu.Unlock()
		c.logf("transcript: %q (final=%v)", m.Text, m.Final)

	case protov2.Display:
		c.mu.Lock()
		c.res.Displays = append(c.res.Displays, m)
		ended := c.speechEnded
		c.mu.Unlock()
		c.logf("display: emotion=%q status=%q", m.Emotion, m.Status)
		// The neutral/listening reset after speech is the end-of-turn marker.
		if m.Status == "listening" && ended {
			c.markComplete()
		}

	case protov2.AudioBegin:
		c.mu.Lock()
		c.speechEnded = false
		c.mu.Unlock()
		c.logf("audio_begin: utterance=%d", m.UtteranceID)

	case protov2.Caption:
		c.mu.Lock()
		c.res.Captions = append(c.res.Captions, m)
		// Cumulative text rides the non-terminal captions; the terminal one
		// (Final=true) omits text and only marks completion (§4.4). So the
		// complete caption is the last non-empty text we saw.
		if m.Text != "" {
			c.res.FinalCaption = m.Text
		}
		c.mu.Unlock()

	case protov2.AudioEnd:
		c.mu.Lock()
		c.speechEnded = true
		c.mu.Unlock()
		c.logf("audio_end: utterance=%d", m.UtteranceID)

	case protov2.AudioCancel:
		c.mu.Lock()
		c.res.Cancelled = true
		c.mu.Unlock()
		c.logf("audio_cancel: utterance=%d (barge-in honoured)", m.UtteranceID)
		c.markComplete()

	case protov2.Alert:
		c.logf("alert: %q — %q", m.Title, m.Message)

	case protov2.System:
		c.logf("system: command=%q", m.Command)

	case protov2.Goodbye:
		g := m
		c.mu.Lock()
		c.res.Goodbye = &g
		c.mu.Unlock()
		c.logf("goodbye: reason=%q", m.Reason)
		c.markComplete()

	case protov2.Error:
		c.mu.Lock()
		c.res.Errors = append(c.res.Errors, m)
		c.mu.Unlock()
		c.logf("error: %s: %s (ref_id=%d)", m.Code, m.Message, m.RefID)

	case protov2.ToolList:
		return c.answerToolList(ctx, m)

	case protov2.ToolCall:
		return c.answerToolCall(ctx, m)

	default:
		c.logf("unhandled frame: %T", msg)
	}
	return nil
}

// answerToolList replies to a discovery request with the configured catalog
// (single page; the reference device has a small, static tool set).
func (c *client) answerToolList(ctx context.Context, req protov2.ToolList) error {
	c.mu.Lock()
	c.res.ToolListReqs++
	c.mu.Unlock()
	// Effective catalog: a device that announced inline (§6.4) but is still
	// asked tool_list should answer with the same set, so fall back to
	// ToolsInline when Tools is unset.
	catalog := c.cfg.Tools
	if len(catalog) == 0 {
		catalog = c.cfg.ToolsInline
	}
	c.logf("tool_list request id=%d → returning %d tools", req.ID, len(catalog))
	return c.writeJSON(ctx, protov2.ToolList{
		Type:   "tool_list",
		ID:     req.ID,
		Result: &protov2.ToolListResult{Tools: catalog},
	})
}

// answerToolCall executes a tool call by returning its canned result (or `true`
// for "ok" when none is configured), mirroring a device that ran the action.
func (c *client) answerToolCall(ctx context.Context, req protov2.ToolCall) error {
	c.mu.Lock()
	c.res.ToolCalls = append(c.res.ToolCalls, req)
	c.mu.Unlock()
	result := json.RawMessage(`true`)
	if r, ok := c.cfg.ToolResults[req.Name]; ok {
		result = r
	}
	c.logf("tool_call %q id=%d args=%s → %s", req.Name, req.ID, string(req.Args), string(result))
	return c.writeJSON(ctx, protov2.ToolCall{
		Type:   "tool_call",
		ID:     req.ID,
		Result: result,
	})
}

// ---- low-level I/O -------------------------------------------------------

func (c *client) writeJSON(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.conn.Write(ctx, websocket.MessageText, data)
}

// readMessage reads the next text frame and decodes it, skipping binary frames.
func (c *client) readMessage(ctx context.Context) (any, error) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		if typ != websocket.MessageText {
			continue
		}
		return decodeServerFrame(data)
	}
}

func byteReader(b []byte) io.Reader { return bytes.NewReader(b) }

func isExpectedClose(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway, websocket.StatusNoStatusRcvd:
		return true
	}
	return false
}
