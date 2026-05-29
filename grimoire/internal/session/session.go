// SPDX-License-Identifier: AGPL-3.0-or-later

// Package session owns the per-WebSocket-connection state machine. Each
// device session is one open WS, with its own session_id, audio negotiation,
// and (eventually) MCP client + ASR/LLM/TTS pipeline state.
//
// For milestone 1 this file does just enough to accept a connection, perform
// the v1 hello handshake, and log subsequent messages until the client
// disconnects. The voice loop will be filled in incrementally.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/audio"
	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/mcp"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
)

// ASR is the speech-recognition interface the session consumes. The real
// implementation is *asr.Whisper (cgo wrapper around whisper.cpp); tests
// can swap in a fake. Go convention: define interfaces where they're
// used, not where they're implemented.
type ASR interface {
	Transcribe(pcm []int16) (string, error)
}

// Config controls the audio params + behavior we advertise back to the
// device in our hello response.
type Config struct {
	// TTSAudio is what we promise to send back as TTS output. The firmware
	// configures its Opus decoder to these params. Recommended:
	// sample_rate=24000, frame_duration=60.
	TTSAudio protocol.AudioParams

	// MicAudio is the format we expect the device's microphone to send.
	// Firmware advertises this in ClientHello.audio_params; the values
	// here are used to construct the per-session Opus decoder before
	// hello so we don't have to lazy-init mid-stream. Defaults to
	// (16000, 1, 60) — the StackChan v1 mic format.
	MicAudio protocol.AudioParams

	// HandshakeTimeout bounds how long we wait for ClientHello after
	// upgrade. Firmware times out at 10s; we should be tighter.
	HandshakeTimeout time.Duration

	// MCPInitTimeout bounds the initialize + tools/list exchange after
	// the WS handshake. 0 → 5s. Best-effort: timing out only means we
	// run without tools, not that the session dies.
	MCPInitTimeout time.Duration

	// ReadIdleTimeout closes the WS if no message of any kind has been
	// received for this long. Firmware times out at 120s; matching prevents
	// half-open sockets piling up. Zero = disabled.
	ReadIdleTimeout time.Duration

	// Kokoro is the TTS provider. nil → Speak() returns an error; useful
	// for unit tests that don't exercise the speaking path.
	Kokoro *tts.KokoroClient

	// ASR is the speech-recognition provider. nil → ASR step is skipped
	// and the turn fizzles.
	ASR ASR

	// LLM is the streaming chat-completions client. nil → LLM step is
	// skipped.
	LLM *llm.Client

	// SystemPrompt, if non-empty, is prepended as a system message on
	// every LLM call. Keeps the persona consistent across turns without
	// bloating the persistent dialogue history.
	SystemPrompt string

	// VisionURL, if non-empty, is advertised to the device in MCP
	// initialize.capabilities.vision.url. The device POSTs camera
	// captures to this URL when the LLM calls self.camera.take_photo.
	VisionURL string

	// VisionToken, if non-empty, is the bearer token the device will
	// include in the Authorization header on vision POSTs. Useful when
	// the vision endpoint is exposed beyond the LAN.
	VisionToken string

	// HardcodedReply, if non-empty, BYPASSES the ASR + LLM pipeline and
	// speaks this text on every listen:stop. Useful for milestone-2-style
	// smoke testing against a real device.
	HardcodedReply string

	// MaxUtteranceMS caps the per-turn microphone buffer to bound memory
	// (and protect against runaway streams). 0 = 30 seconds. Hitting the
	// cap force-endpoints the turn rather than dropping audio forever.
	MaxUtteranceMS int

	// VAD tunes the server-side endpoint detector used in auto-stop mode.
	// Zero fields take sensible defaults (see EndpointConfig.withDefaults).
	VAD EndpointConfig

	// BeepTrim removes the wake-acknowledge beep from the front of each
	// capture before ASR. Disabled when MaxScanMS <= 0.
	BeepTrim BeepTrimConfig

	// TimeLocation is the timezone the get_current_time server tool reports
	// in. nil → UTC. The container runs in UTC, so set this to the user's
	// zone (e.g. America/New_York) for sensible answers.
	TimeLocation *time.Location

	// DebugDumpDir, if non-empty, makes each captured turn's PCM get written
	// there as a 16 kHz mono WAV before transcription. Pure diagnostics —
	// lets us play back exactly what the server fed to whisper.
	DebugDumpDir string

	// Logger receives per-session events. nil → slog.Default().
	Logger *slog.Logger
}

// Handler returns an http.HandlerFunc that upgrades incoming requests to
// WebSocket and runs a session on each connection. Blocking until the client
// disconnects; HTTP server should be in a goroutine per connection (which
// Go's net/http does by default).
func Handler(cfg Config) http.HandlerFunc {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 8 * time.Second
	}
	if cfg.TTSAudio.SampleRate == 0 {
		cfg.TTSAudio = protocol.AudioParams{SampleRate: 24000, FrameDuration: 60}
	}
	if cfg.MicAudio.SampleRate == 0 {
		cfg.MicAudio = protocol.AudioParams{SampleRate: 16000, Channels: 1, FrameDuration: 60}
	}
	if cfg.MaxUtteranceMS == 0 {
		cfg.MaxUtteranceMS = 30000
	}
	if cfg.MCPInitTimeout == 0 {
		cfg.MCPInitTimeout = 5 * time.Second
	}

	return func(w http.ResponseWriter, r *http.Request) {
		log := cfg.Logger.With(
			"remote", r.RemoteAddr,
			"device_id", r.Header.Get("Device-Id"),
			"client_id", r.Header.Get("Client-Id"),
		)

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// Insecure here means we don't enforce same-origin; the device
			// has no concept of Origin.
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Warn("ws upgrade failed", "err", err)
			return
		}
		// 1 MiB is comfortable headroom for any control message. Audio
		// frames are ~80-200 bytes typically.
		conn.SetReadLimit(1 << 20)

		// One Opus encoder per session (encoder is stateful). Configured
		// to match the audio params we advertised in hello.
		enc, err := audio.NewEncoder(cfg.TTSAudio.SampleRate, 1, cfg.TTSAudio.FrameDuration)
		if err != nil {
			log.Error("opus encoder init failed", "err", err)
			_ = conn.Close(websocket.StatusInternalError, "encoder init")
			return
		}
		// And a mirror decoder for incoming mic audio.
		dec, err := audio.NewDecoder(cfg.MicAudio.SampleRate, cfg.MicAudio.Channels, cfg.MicAudio.FrameDuration)
		if err != nil {
			log.Error("opus decoder init failed", "err", err)
			_ = conn.Close(websocket.StatusInternalError, "decoder init")
			return
		}

		// Take the lifetime of this session from the request context so
		// shutdown propagates cleanly.
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		// Pre-size the mic buffer to MaxUtteranceMS worth of PCM bytes.
		maxBytes := cfg.MicAudio.SampleRate * cfg.MicAudio.Channels * 2 * cfg.MaxUtteranceMS / 1000

		s := &Session{
			conn:      conn,
			cfg:       cfg,
			log:       log,
			closed:    make(chan struct{}),
			encoder:   enc,
			decoder:   dec,
			micBufMax: maxBytes,
			micBuf:    make([]byte, 0, maxBytes),
		}
		s.localTools, s.localHandlers = s.buildLocalTools()
		s.run(ctx)
	}
}

// Session is one open WebSocket connection. Not safe for concurrent use
// externally; all socket reads happen on the run() goroutine. Writes use
// writeJSON which takes an internal lock.
type Session struct {
	conn      *websocket.Conn
	cfg       Config
	log       *slog.Logger
	sessionID string
	closed    chan struct{}
	encoder   *audio.Encoder
	decoder   *audio.Decoder

	// micBuf accumulates decoded PCM (s16 little-endian) during a listen
	// turn. Bounded by micBufMax to keep one rogue session from eating
	// all RAM. Reset on every listen:start.
	micBuf     []byte
	micBufMax  int
	listening  bool
	micDropped bool // true once we hit micBufMax and started dropping

	// turnSeq numbers captured turns for debug WAV dump filenames.
	turnSeq atomic.Int64

	// ep is the server-side endpoint detector for the current listen turn.
	// Non-nil only while listening in auto-stop mode (the device streams
	// continuously and never sends listen:stop itself). nil in manual mode,
	// where the device sends listen:stop on its own.
	ep *endpointer

	// autoStop records whether the current listen window is auto-stop mode
	// (server-side endpointing). Used to rebuild the endpointer when we
	// re-arm listening. Written only on the read-loop goroutine.
	autoStop bool

	// rearm is set by handleTurn (a separate goroutine) when a turn finishes
	// without a spoken reply. In auto mode the device keeps streaming while
	// in its Listening state, so the read loop resumes endpointing the
	// ongoing audio on the next frame — otherwise voice gets stuck (the
	// device never sends a fresh listen:start without a Speaking transition).
	// An atomic so the turn goroutine can signal without touching listening
	// state directly (which only the read-loop goroutine mutates).
	rearm atomic.Bool

	// speakMu is held for the full duration of a TTS response so two
	// Speak() calls can't interleave their tts:start / frames / tts:stop
	// sequences on the wire.
	speakMu sync.Mutex

	// dialogue is the running chat-completions message history for this
	// session. dialogueMu guards it because handleTurn runs concurrently
	// with the read loop (which may also append to dialogue if/when we
	// add events).
	dialogueMu sync.Mutex
	dialogue   []llm.Message

	// mcpClient is set after a successful hello if Kokoro/LLM/ASR are
	// configured. Used to query the device's tool registry and to
	// dispatch tool calls during a turn.
	mcpClient *mcp.Client

	// tools is the list of tool descriptors fetched from the device.
	// Pre-converted to llm.Tool format for fast injection into LLM
	// requests. Empty if MCP is disabled or the device exposed no tools.
	toolsMu sync.RWMutex
	tools   []llm.Tool
	// toolByName indexes tools so the LLM-emitted tool_call name → tool
	// lookup is cheap (used for permission checks etc.).
	toolByName map[string]mcp.ToolDescriptor

	// localTools are server-side tools (e.g. get_current_time) advertised to
	// the LLM alongside the device's MCP tools. localHandlers dispatches them
	// without going over MCP. Both set once at construction, read-only after.
	localTools    []llm.Tool
	localHandlers map[string]localToolHandler
}

func (s *Session) run(ctx context.Context) {
	defer close(s.closed)
	defer s.conn.Close(websocket.StatusNormalClosure, "")

	// Step 1: handshake. Waits for ClientHello, sends ServerHello.
	if err := s.handshake(ctx); err != nil {
		s.log.Warn("handshake failed", "err", err)
		_ = s.conn.Close(websocket.StatusPolicyViolation, "handshake")
		return
	}

	s.log.Info("session established", "session_id", s.sessionID)

	// Step 2: spin up MCP initialization in a goroutine. initMCP needs
	// the read loop running to receive its responses, so we can't block
	// here. handleTurn fetches tools lazily via snapshotTools() and is
	// fine with an empty list until initMCP finishes.
	if s.cfg.LLM != nil {
		go s.initMCP(ctx)
	}

	// Step 3: read loop.
	s.readLoop(ctx)

	if s.mcpClient != nil {
		s.mcpClient.Close()
	}
	s.log.Info("session closed", "session_id", s.sessionID)
}

// initMCP runs the MCP initialize handshake against the device and pulls
// the tool list. Stores the result on the session. Failures are logged
// but non-fatal — the voice loop still works without tools.
//
// MUST run on a goroutine, NOT in the main session flow, because the
// MCP responses arrive on the read loop which has to already be
// pumping frames.
func (s *Session) initMCP(ctx context.Context) {
	// Set the client first so the read loop can route incoming MCP
	// frames to it as soon as they start arriving (the device often
	// sends MCP responses with very low latency).
	client := mcp.NewClient(func(ctx context.Context, payload []byte) error {
		return writeJSON(ctx, s.conn, protocol.MCP{
			SessionID: s.sessionID,
			Type:      "mcp",
			Payload:   payload,
		})
	})
	s.mcpClient = client

	initCtx, cancel := context.WithTimeout(ctx, s.cfg.MCPInitTimeout)
	defer cancel()

	var capabilities json.RawMessage
	if s.cfg.VisionURL != "" {
		vis := map[string]string{"url": s.cfg.VisionURL}
		if s.cfg.VisionToken != "" {
			vis["token"] = s.cfg.VisionToken
		}
		capabilities, _ = json.Marshal(map[string]any{"vision": vis})
	}

	if _, err := client.Initialize(initCtx, capabilities); err != nil {
		s.log.Warn("mcp initialize failed; running without tools", "err", err)
		return
	}

	descs, err := client.ListTools(initCtx, false)
	if err != nil {
		s.log.Warn("mcp tools/list failed; running without tools", "err", err)
		return
	}

	tools := make([]llm.Tool, 0, len(descs))
	byName := make(map[string]mcp.ToolDescriptor, len(descs))
	for _, d := range descs {
		tools = append(tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.InputSchema,
			},
		})
		byName[d.Name] = d
	}
	s.toolsMu.Lock()
	s.tools = tools
	s.toolByName = byName
	s.toolsMu.Unlock()

	s.log.Info("mcp ready", "tools", len(tools))
}

// snapshotTools returns a copy of the current tool list (safe to pass
// to a long-running LLM stream).
func (s *Session) snapshotTools() []llm.Tool {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	// Device (MCP) tools first, then server-side tools. localTools is set
	// once at construction so it's safe to read here.
	out := make([]llm.Tool, 0, len(s.tools)+len(s.localTools))
	out = append(out, s.tools...)
	out = append(out, s.localTools...)
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Session) handshake(ctx context.Context) error {
	hsCtx, cancel := context.WithTimeout(ctx, s.cfg.HandshakeTimeout)
	defer cancel()

	mt, data, err := s.conn.Read(hsCtx)
	if err != nil {
		return fmt.Errorf("read client hello: %w", err)
	}
	if mt != websocket.MessageText {
		return fmt.Errorf("first frame must be text JSON, got %v", mt)
	}

	msg, err := protocol.Decode(data)
	if err != nil {
		return fmt.Errorf("decode client hello: %w", err)
	}
	hello, ok := msg.(protocol.ClientHello)
	if !ok {
		return fmt.Errorf("first frame must be type=hello, got %T", msg)
	}

	s.log.Info("client hello",
		"version", hello.Version,
		"transport", hello.Transport,
		"mic_rate", helloRate(hello.AudioParams),
		"features", helloFeatures(hello.Features),
	)

	s.sessionID = newSessionID()
	ttsAudio := s.cfg.TTSAudio

	resp := protocol.ServerHello{
		Type:        "hello",
		Transport:   "websocket",
		SessionID:   s.sessionID,
		AudioParams: &ttsAudio,
	}
	return writeJSON(hsCtx, s.conn, resp)
}

func (s *Session) readLoop(ctx context.Context) {
	for {
		readCtx := ctx
		if s.cfg.ReadIdleTimeout > 0 {
			var cancel context.CancelFunc
			readCtx, cancel = context.WithTimeout(ctx, s.cfg.ReadIdleTimeout)
			defer cancel()
		}

		mt, data, err := s.conn.Read(readCtx)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return
			}
			// websocket.CloseStatus returns -1 for non-close errors; for
			// normal close it returns the close code.
			if cs := websocket.CloseStatus(err); cs != -1 {
				s.log.Info("ws closed by client", "code", cs)
				return
			}
			s.log.Warn("read error", "err", err)
			return
		}

		switch mt {
		case websocket.MessageBinary:
			s.onAudioFrame(data)
		case websocket.MessageText:
			s.dispatchText(ctx, data)
		}
	}
}

// onAudioFrame is called for every incoming binary WS frame (one Opus
// packet). When we're in a listening window, decode it into PCM and
// accumulate into micBuf, bounded by micBufMax.
// armListen (re)starts a listening window: clears the mic buffer and builds a
// fresh endpoint detector for auto-stop mode. Read-loop goroutine only.
func (s *Session) armListen() {
	s.micBuf = s.micBuf[:0]
	s.micDropped = false
	s.listening = true
	if s.autoStop {
		cfg := s.cfg.VAD
		cfg.FrameMS = s.cfg.MicAudio.FrameDuration
		s.ep = newEndpointer(cfg)
	} else {
		s.ep = nil
	}
}

func (s *Session) onAudioFrame(opusBytes []byte) {
	if !s.listening {
		// We're between turns. If the previous turn produced no spoken reply,
		// the device (in auto mode) is still in its Listening state streaming
		// audio — but it won't send a fresh listen:start without a Speaking
		// transition, so we'd never re-engage. Resume endpointing the ongoing
		// stream. The CAS fires once per pending re-arm, on this goroutine.
		if s.rearm.CompareAndSwap(true, false) {
			s.log.Debug("re-arming listening (previous turn had no reply; device still streaming)")
			s.armListen()
		} else {
			// Frame arrived just after a turn fired / during our TTS. Discard.
			return
		}
	}
	if len(s.micBuf) >= s.micBufMax {
		// Buffer cap reached. Rather than drop audio forever (which would
		// hang the turn), treat it as a forced endpoint and process what
		// we have.
		s.log.Warn("mic buffer full; forcing endpoint", "limit_bytes", s.micBufMax)
		s.fireTurn("buffer-full")
		return
	}
	before := len(s.micBuf)
	var err error
	s.micBuf, err = s.decoder.DecodeAppend(s.micBuf, opusBytes)
	if err != nil {
		s.log.Warn("opus decode failed; dropping frame", "err", err, "bytes", len(opusBytes))
		return
	}
	// Auto-stop mode: run the endpoint detector over the newly decoded
	// samples. When it fires, the user has stopped talking and we run the
	// turn. (Manual mode leaves s.ep nil and waits for a device listen:stop.)
	if s.ep != nil {
		fired := s.ep.update(bytesToInt16(s.micBuf[before:]))
		// Per-frame envelope; verbose but debug-only and only while
		// listening. Lets us read the actual speech/silence levels off a
		// real turn to tune thresholds.
		s.log.Debug("mic frame",
			"energy", int(s.ep.lastEnergy),
			"thresh", int(s.ep.lastThreshold),
			"floor", int(s.ep.floor),
			"speech_ms", s.ep.speechMS,
			"silence_ms", s.ep.silenceMS)
		if fired {
			s.fireTurn("server-vad")
		}
	}
}

func (s *Session) dispatchText(_ context.Context, data []byte) {
	msg, err := protocol.Decode(data)
	if err != nil {
		s.log.Warn("decode failed", "err", err, "raw", string(data))
		return
	}
	switch m := msg.(type) {
	case protocol.ClientHello:
		s.log.Warn("duplicate hello received")
	case protocol.Listen:
		s.log.Info("listen", "state", m.State, "mode", m.Mode, "wake_text", m.Text)
		switch m.State {
		case "start":
			// Device-driven listen start supersedes any pending re-arm.
			// auto-stop mode (or unspecified, which the firmware uses as the
			// AEC-off default) means the device streams continuously and never
			// sends listen:stop — we endpoint server-side. Manual mode sends
			// its own listen:stop, so skip the detector.
			s.autoStop = m.Mode == "auto" || m.Mode == ""
			s.rearm.Store(false)
			s.armListen()
		case "stop":
			// Device-initiated stop (manual mode / barge-in). Fire the turn.
			s.fireTurn("device-stop")
		case "detect":
			// Wake word fired. Currently informational; voice loop
			// triggers on the subsequent listen:start.
		}
	case protocol.Abort:
		s.log.Info("abort", "reason", m.Reason)
	case protocol.MCP:
		if s.mcpClient != nil {
			s.mcpClient.HandleIncoming(m.Payload)
		} else {
			s.log.Debug("mcp message but no client", "bytes", len(m.Payload))
		}
	case protocol.Event:
		s.log.Info("event", "name", m.Name)
	case protocol.Unknown:
		s.log.Info("unknown message type", "type", m.Type)
	}
}

// Helpers --------------------------------------------------------------------

func newSessionID() string {
	// Short hex is plenty unique for the lifetime of a device session and
	// reads better in logs than a full UUID.
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func helloRate(p *protocol.AudioParams) int {
	if p == nil {
		return 0
	}
	return p.SampleRate
}

func helloFeatures(f *protocol.Features) string {
	if f == nil {
		return ""
	}
	out := ""
	if f.MCP {
		out += "mcp,"
	}
	if f.AEC {
		out += "aec,"
	}
	if out != "" {
		out = out[:len(out)-1]
	}
	return out
}
