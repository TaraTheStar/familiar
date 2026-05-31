// SPDX-License-Identifier: AGPL-3.0-or-later

// Package v2client is a reference Protocol v2 device client: it speaks the real
// v2 wire (docs/PROTOCOL_V2.md) over a real WebSocket against a grimoire server.
//
// It exists to prove the v2 server contract end to end without firmware — the
// full handshake (hello + clock sync + flow control), Opus interop in both
// directions, credit-based audio flow control, first-class tool discovery and
// calls, captions, and barge-in — and to serve as the conformance oracle the
// ESP32 firmware port is checked against. The voice loop's behaviour is driven
// by the server; this client just renders one turn the way a device would.
//
// The package is deliberately laptop-runnable: a single turn is driven from an
// in-memory or WAV PCM buffer, the server's TTS is decoded back to PCM, and the
// whole exchange is summarised in a TurnResult for tests and the CLI.
package v2client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/audio"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
)

// Config drives a single reference turn.
type Config struct {
	// URL is the v2 WebSocket endpoint, e.g. ws://192.0.2.10:9098/grimoire/.
	URL string

	// MicPCM is the user utterance as 16-bit little-endian mono PCM at MicRate.
	// It is encoded to Opus and streamed as the turn's microphone audio.
	MicPCM  []byte
	MicRate int // mic sample rate (Hz); 0 → 16000

	// Preroll, when non-empty, is wake_word_audio pre-roll (§4.2): the client
	// advertises the wake_word_audio feature, sends a `wake`, then streams this
	// PCM as binary frames before listen_start so the server's window opens at
	// wake. Same format as MicPCM. Empty = no pre-roll (window opens at
	// listen_start, the default).
	Preroll []byte

	// Tools is the device tool catalog the client answers tool_list discovery
	// with. Empty means the device exposes no tools (a valid, common case).
	Tools []protov2.ToolDescriptor
	// ToolsInline, when set, is announced in the hello's client.tools_inline
	// (§6.4) so the server can skip tool_list discovery. The client still
	// answers a tool_list if one arrives anyway (using ToolsInline as the
	// catalog when Tools is empty), so a non-skipping server stays functional.
	ToolsInline []protov2.ToolDescriptor
	// ToolResults are canned results keyed by tool name, returned for tool_call.
	// A tool not present here returns JSON `true` ("ok").
	ToolResults map[string]json.RawMessage

	// BargeAfterFrames, when > 0, sends an `abort` after that many TTS audio
	// frames have been received — exercising the barge-in path (§4.4). The
	// server should respond with audio_cancel.
	BargeAfterFrames int

	// CreditBatch is how many consumed TTS frames the client lets accumulate
	// before granting that many audio_credit back (§5.2). 0 → 8.
	CreditBatch int

	// OutWAV, if set, is where the decoded server TTS is written as a WAV.
	OutWAV string

	// Logf, if set, receives human-readable progress lines.
	Logf func(format string, args ...any)
}

// TurnResult is the protocol-agnostic summary of one rendered turn.
type TurnResult struct {
	ServerHello protov2.ServerHello

	Transcript   string
	Displays     []protov2.Display
	Captions     []protov2.Caption
	FinalCaption string

	AudioFrames int    // binary TTS frames received
	AudioPCM    []byte // decoded little-endian s16 mono

	ToolListReqs int                // tool_list discovery requests answered
	ToolCalls    []protov2.ToolCall // tool_call requests the server dispatched

	Cancelled bool             // saw audio_cancel (barge-in honoured)
	Goodbye   *protov2.Goodbye // graceful farewell, if any
	Errors    []protov2.Error  // first-class error frames (§4.9)
}

// Run dials the server, performs the v2 handshake, drives exactly one turn from
// cfg.MicPCM, and returns once the turn completes (the avatar returns to
// listening), a barge-in is honoured, a goodbye arrives, or ctx is cancelled.
func Run(ctx context.Context, cfg Config) (*TurnResult, error) {
	c, err := dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer c.conn.CloseNow()

	if err := c.hello(ctx); err != nil {
		return nil, err
	}

	// Reader runs concurrently: it answers discovery, returns audio credit,
	// decodes TTS, and watches for the end-of-turn marker.
	go c.readLoop(ctx)

	if err := c.sendTurn(ctx); err != nil {
		return nil, err
	}

	// Wait for the turn to resolve, then close gracefully and let the reader
	// drain so the result is race-free.
	select {
	case <-c.turnComplete:
	case <-ctx.Done():
	}
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
	<-c.readerExited

	c.mu.Lock()
	res := c.res
	c.mu.Unlock()

	if cfg.OutWAV != "" && len(res.AudioPCM) > 0 {
		if err := writeWAV(cfg.OutWAV, res.AudioPCM, c.dec.SampleRate()); err != nil {
			c.logf("warning: writing %s: %v", cfg.OutWAV, err)
		} else {
			c.logf("wrote %d decoded TTS frames to %s", res.AudioFrames, cfg.OutWAV)
		}
	}
	return &res, ctx.Err()
}

type client struct {
	cfg  Config
	conn *websocket.Conn
	enc  *audio.Encoder // mic: server-confirmed in-stream format
	dec  *audio.Decoder // tts: server-confirmed out-stream format

	nextID int // client-originated request ids (none yet, reserved)

	mu  sync.Mutex
	res TurnResult

	credit       int  // server→device frames consumed since last grant
	speechEnded  bool // audio_end seen for the current utterance
	bargeSent    bool
	turnComplete chan struct{}
	completeOnce sync.Once
	readerExited chan struct{}
}

func dial(ctx context.Context, cfg Config) (*client, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("v2client: URL is required")
	}
	conn, _, err := websocket.Dial(ctx, cfg.URL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Protocol-Version": []string{"2"}},
	})
	if err != nil {
		return nil, fmt.Errorf("v2client: dial %s: %w", cfg.URL, err)
	}
	// TTS frames can be large after many sentences; lift the default read limit.
	conn.SetReadLimit(1 << 20)
	return &client{
		cfg:          cfg,
		conn:         conn,
		turnComplete: make(chan struct{}),
		readerExited: make(chan struct{}),
	}, nil
}

func (c *client) logf(format string, args ...any) {
	if c.cfg.Logf != nil {
		c.cfg.Logf(format, args...)
	}
}

// hello performs the §3 handshake and binds the Opus codecs to the audio format
// the server dictates in its reply.
func (c *client) hello(ctx context.Context) error {
	micRate := c.cfg.MicRate
	if micRate == 0 {
		micRate = 16000
	}
	c.nextID++
	// "tools" = device supports first-class tool discovery/calls. (v1's "mcp"
	// token is retired in v2 — there is no JSON-RPC envelope here.) Advertise
	// wake_word_audio when a pre-roll is configured so the server opens the
	// audio window at wake (§4.2).
	features := []string{"tools"}
	if len(c.cfg.Preroll) > 0 {
		features = append(features, "wake_word_audio")
	}
	hello := protov2.ClientHello{
		Type:   "hello",
		ID:     c.nextID,
		Client: protov2.ClientInfo{Name: "v2client", Version: "0.1.0", ToolsInline: c.cfg.ToolsInline},
		Audio: protov2.AudioConfig{
			In:  protov2.AudioStream{Codec: "opus", Rate: micRate, Channels: 1, FrameMS: 60},
			Out: protov2.AudioStream{Codec: "opus", Rate: 24000, Channels: 1, FrameMS: 60},
		},
		Features: features,
	}
	if err := c.writeJSON(ctx, hello); err != nil {
		return fmt.Errorf("v2client: send hello: %w", err)
	}

	msg, err := c.readMessage(ctx)
	if err != nil {
		return fmt.Errorf("v2client: read server hello: %w", err)
	}
	sh, ok := msg.(protov2.ServerHello)
	if !ok {
		return fmt.Errorf("v2client: first server frame was %T, want hello", msg)
	}
	if sh.Error != nil {
		return fmt.Errorf("v2client: server rejected hello: %s: %s", sh.Error.Code, sh.Error.Message)
	}
	if sh.Result != "ok" {
		return fmt.Errorf("v2client: server hello result = %q, want \"ok\"", sh.Result)
	}
	c.res.ServerHello = sh

	// Bind codecs to the server-dictated formats (§3.2 "Dictate"). Fall back to
	// the standard 16k mic / 24k TTS if the server leaves a field implicit.
	inRate, inFrame := 16000, 60
	outRate, outFrame := 24000, 60
	if a := sh.Audio; a != nil {
		if a.In.Rate != 0 {
			inRate = a.In.Rate
		}
		if a.In.FrameMS != 0 {
			inFrame = a.In.FrameMS
		}
		if a.Out.Rate != 0 {
			outRate = a.Out.Rate
		}
		if a.Out.FrameMS != 0 {
			outFrame = a.Out.FrameMS
		}
	}
	if c.enc, err = audio.NewEncoder(inRate, 1, inFrame); err != nil {
		return fmt.Errorf("v2client: mic encoder: %w", err)
	}
	if c.dec, err = audio.NewDecoder(outRate, 1, outFrame); err != nil {
		return fmt.Errorf("v2client: tts decoder: %w", err)
	}

	credit := 0
	if sh.FlowControl != nil {
		credit = sh.FlowControl.AudioCreditInitial
	}
	c.logf("hello ok: server=%s/%s, mic=%dHz tts=%dHz, audio_credit=%d, features=%v",
		serverName(sh), serverVersion(sh), inRate, outRate, credit, sh.Features)
	return nil
}

func serverName(sh protov2.ServerHello) string {
	if sh.Server != nil {
		return sh.Server.Name
	}
	return "?"
}

func serverVersion(sh protov2.ServerHello) string {
	if sh.Server != nil {
		return sh.Server.Version
	}
	return "?"
}

// sendTurn streams the configured mic PCM as one capture window: listen_start,
// Opus frames, listen_stop. With MicPCM empty it still opens/closes the window
// (a silent turn), which is enough to exercise the handshake.
func (c *client) sendTurn(ctx context.Context) error {
	// wake_word_audio pre-roll (§4.2): announce wake, then stream the buffered
	// pre-roll as binary frames before listen_start, so the server opens the
	// turn's audio window at wake and the pre-roll counts as turn audio.
	if len(c.cfg.Preroll) > 0 {
		if err := c.writeJSON(ctx, protov2.Wake{Type: "wake", Phrase: "hi_stackchan", Score: 0.9}); err != nil {
			return fmt.Errorf("v2client: wake: %w", err)
		}
		pre, err := c.streamPCM(ctx, c.cfg.Preroll)
		if err != nil {
			return fmt.Errorf("v2client: preroll: %w", err)
		}
		c.logf("sent %d pre-roll frames", pre)
	}
	if err := c.writeJSON(ctx, protov2.ListenStart{Type: "listen_start", Mode: "auto"}); err != nil {
		return fmt.Errorf("v2client: listen_start: %w", err)
	}

	frames := 0
	if len(c.cfg.MicPCM) > 0 {
		framer := audio.NewPCMFramer(byteReader(c.cfg.MicPCM), c.enc.SamplesPerFrame())
		for framer.Next() {
			pkt, err := c.enc.Encode(framer.Frame())
			if err != nil {
				return fmt.Errorf("v2client: encode mic frame: %w", err)
			}
			out := make([]byte, len(pkt)) // copy: Encode reuses its scratch buffer
			copy(out, pkt)
			if err := c.conn.Write(ctx, websocket.MessageBinary, out); err != nil {
				return fmt.Errorf("v2client: send mic frame: %w", err)
			}
			frames++
		}
		if err := framer.Err(); err != nil {
			return fmt.Errorf("v2client: read mic PCM: %w", err)
		}
	}

	if err := c.writeJSON(ctx, protov2.ListenStop{Type: "listen_stop"}); err != nil {
		return fmt.Errorf("v2client: listen_stop: %w", err)
	}
	c.logf("sent turn: %d mic frames", frames)
	return nil
}

// streamPCM encodes pcm to Opus and writes each frame as a binary WS frame,
// returning the frame count. Shared by the pre-roll and live-mic paths.
func (c *client) streamPCM(ctx context.Context, pcm []byte) (int, error) {
	framer := audio.NewPCMFramer(byteReader(pcm), c.enc.SamplesPerFrame())
	n := 0
	for framer.Next() {
		pkt, err := c.enc.Encode(framer.Frame())
		if err != nil {
			return n, fmt.Errorf("encode frame: %w", err)
		}
		out := make([]byte, len(pkt)) // copy: Encode reuses its scratch buffer
		copy(out, pkt)
		if err := c.conn.Write(ctx, websocket.MessageBinary, out); err != nil {
			return n, fmt.Errorf("send frame: %w", err)
		}
		n++
	}
	return n, framer.Err()
}

func (c *client) creditBatch() int {
	if c.cfg.CreditBatch > 0 {
		return c.cfg.CreditBatch
	}
	return 8
}

func (c *client) markComplete() {
	c.completeOnce.Do(func() { close(c.turnComplete) })
}
