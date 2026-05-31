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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/TaraTheStar/familiar/grimoire/internal/audio"
	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/protov2"
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

	// AudioCreditInitial is the Protocol v2 server→device audio flow-control
	// budget advertised in the hello: how many binary audio frames the server
	// may send before any credit refill (PROTOCOL_V2 §5.1). 0 → 40 frames
	// (≈2.4s at 60ms, matching the ESP32 decoder buffer). Ignored under v1,
	// which paces audio against the wall clock instead.
	AudioCreditInitial int

	// TTSTailPadMS is how much trailing silence (ms) to append to each v2 reply
	// before audio_end. v2 has no wall-clock drain (unlike v1's ttsLead), so the
	// device can leave the speaking state just before its buffer empties and
	// clip the last word. 0 (the zero value) → no pad, so v2 stays frame-for-frame
	// equivalent to v1 by default; the binary opts in (400ms mirrors v1's
	// ttsLead). v1 ignores it.
	TTSTailPadMS int

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
	if cfg.ReadIdleTimeout == 0 {
		// Match the firmware's 120s timeout: close half-open sockets instead of
		// leaking them. v2 uses this as its liveness mechanism in place of WS
		// ping/pong (PROTOCOL_V2 §3.4).
		cfg.ReadIdleTimeout = 120 * time.Second
	}

	return func(w http.ResponseWriter, r *http.Request) {
		log := cfg.Logger.With(
			"remote", r.RemoteAddr,
			"device_id", r.Header.Get("Device-Id"),
			"client_id", r.Header.Get("Client-Id"),
		)

		// Select the wire protocol from the upgrade header before accepting.
		// Only Protocol-Version 2 is supported (v1 was removed — see
		// docs/PROTOCOL_V2.md §10 Phase 4). An empty header is tolerated as v2
		// for the current fleet; anything else gets 426 Upgrade Required
		// (PROTOCOL_V2 §2.2) rather than upgrading into a protocol we can't speak.
		if hv := strings.TrimSpace(r.Header.Get("Protocol-Version")); hv != "" && hv != "2" {
			log.Warn("rejecting unsupported protocol version", "header", hv)
			http.Error(w, "unsupported Protocol-Version (expected 2)", http.StatusUpgradeRequired)
			return
		}

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

	// wakeWordAudio records that the device advertised the wake_word_audio
	// feature in its hello (PROTOCOL_V2 §4.2): a wake from idle opens the turn's
	// audio window early so buffered pre-roll counts as turn audio. Set once in
	// handshakeV2, read-only after.
	wakeWordAudio bool
	// prerollOpen is true while a window opened by a wake pre-roll is awaiting
	// its listen_start. In this state mic frames buffer but no endpoint detector
	// runs (the mode isn't known until listen_start, and the pre-roll must not
	// self-endpoint). listen_start keeps the buffered pre-roll and attaches the
	// detector. Read-loop owned.
	prerollOpen bool

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

	// turnCancel cancels the in-flight turn's context for barge-in (abort) and
	// session teardown. Set in fireTurn, invoked by cancelTurn — both on the
	// read-loop goroutine, so it needs no lock. nil between turns.
	turnCancel context.CancelFunc

	// out is the protocol-specific output sink (deviceOut) the voice loop
	// sends through; dec maps inbound wire frames to normalized inEvents.
	// Both are set after the handshake to the implementation selected by the
	// Protocol-Version upgrade header (v1Out / v1Decoder today). The loop
	// never touches a protocol.* type or the raw socket directly — see wire.go.
	out deviceOut
	dec wireDecoder

	// dialogue is the running chat-completions message history for this
	// session. dialogueMu guards it because handleTurn runs concurrently
	// with the read loop (which may also append to dialogue if/when we
	// add events).
	dialogueMu sync.Mutex
	dialogue   []llm.Message

	// toolPort is the device's tool registry, bound after the handshake to the
	// implementation for the active protocol (v1ToolPort over MCP, v2ToolPort
	// over first-class tool messages — see tools.go). nil if LLM is unconfigured.
	// The loop discovers and calls device tools only through it.
	toolPort toolPort

	// tools is the device tool catalog discovered by initTools, pre-converted
	// to llm.Tool format for fast injection into LLM requests. Empty until
	// discovery finishes, or if the device exposed no tools.
	toolsMu sync.RWMutex
	tools   []llm.Tool
	// toolByName indexes the discovered tools by name (carries permission for
	// future LLM-exposure filtering).
	toolByName map[string]toolDescriptor

	// inlineTools holds device tools announced up front in the v2 hello
	// (tools_inline, PROTOCOL_V2 §6.4). When it carries ≥1 usable descriptor,
	// initTools registers it and skips the tool_list round-trip; if it is
	// present but unusable, initTools falls back to tool_list discovery rather
	// than run toolless (belt-and-suspenders). Set in handshakeV2 before
	// initTools is spawned, so the goroutine handoff is the happens-before edge
	// and no mutex is needed.
	inlineTools []toolDescriptor

	// localTools are server-side tools (e.g. get_current_time) advertised to
	// the LLM alongside the device's MCP tools. localHandlers dispatches them
	// without going over MCP. Both set once at construction, read-only after.
	localTools    []llm.Tool
	localHandlers map[string]localToolHandler
}

func (s *Session) run(ctx context.Context) {
	defer close(s.closed)
	defer s.conn.Close(websocket.StatusNormalClosure, "")

	// Step 1: v2 handshake, then bind the protocol seam. The voice loop
	// (turn.go, mic.go) talks only to s.out / s.dec (see wire.go).
	if err := s.handshakeV2(ctx); err != nil {
		s.log.Warn("v2 handshake failed", "err", err)
		_ = s.conn.Close(websocket.StatusPolicyViolation, "handshake")
		return
	}
	s.out = newV2Out(s.conn, s.encoder, s.log, s.audioCredit(), s.tailPadFrames())
	s.dec = v2Decoder{}
	if s.cfg.LLM != nil {
		s.toolPort = newV2ToolPort(s.conn, s.log)
	}

	s.log.Info("session established", "session_id", s.sessionID)

	// Step 2: discover the device tool catalog in a goroutine. initTools waits
	// on responses that arrive on the read loop, so it can't block here; the
	// port is already bound (above) so inbound frames route correctly the moment
	// they arrive. handleTurn reads tools lazily via snapshotTools and is fine
	// with an empty list until discovery finishes.
	if s.toolPort != nil {
		go s.initTools(ctx)
	}

	// Step 3: read loop.
	s.readLoop(ctx)

	if s.toolPort != nil {
		s.toolPort.Close()
	}
	s.log.Info("session closed", "session_id", s.sessionID)
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

// handshakeV2 performs the Protocol v2 hello exchange (PROTOCOL_V2 §3): read the
// device's hello, reply with the negotiated audio params and the initial audio
// credit. v2 drops the per-message session_id (the connection is the session),
// so the minted id is for logs/correlation only and never goes on the wire.
func (s *Session) handshakeV2(ctx context.Context) error {
	hsCtx, cancel := context.WithTimeout(ctx, s.cfg.HandshakeTimeout)
	defer cancel()

	mt, data, err := s.conn.Read(hsCtx)
	if err != nil {
		return fmt.Errorf("read client hello: %w", err)
	}
	if mt != websocket.MessageText {
		return fmt.Errorf("first frame must be text JSON, got %v", mt)
	}

	msg, err := protov2.Decode(data)
	if err != nil {
		return fmt.Errorf("decode client hello: %w", err)
	}
	hello, ok := msg.(protov2.ClientHello)
	if !ok {
		return fmt.Errorf("first frame must be type=hello, got %T", msg)
	}

	s.log.Info("client hello",
		"version", 2,
		"client", hello.Client.Name,
		"mic_rate", hello.Audio.In.Rate,
		"features", strings.Join(hello.Features, ","),
		"tools_inline", len(hello.Client.ToolsInline),
	)

	// wake_word_audio (§4.2): record whether the device streams buffered mic
	// pre-roll on wake, so the read loop opens the audio window at wake rather
	// than listen_start. Off by default in firmware; the server honors either
	// ordering.
	for _, f := range hello.Features {
		if f == "wake_word_audio" {
			s.wakeWordAudio = true
			break
		}
	}

	// tools_inline (§6.4): if the device announced its tool catalog in the
	// hello, normalize and stash it so initTools can skip the tool_list
	// round-trip. The fast-path/fallback decision lives in initTools.
	if n := len(hello.Client.ToolsInline); n > 0 {
		inline := make([]toolDescriptor, 0, n)
		for _, d := range hello.Client.ToolsInline {
			inline = append(inline, toolDescriptorFromV2(d))
		}
		s.inlineTools = inline
	}

	s.sessionID = newSessionID()
	tts := s.cfg.TTSAudio
	mic := s.cfg.MicAudio

	// Server capabilities advertised to the device. "tools" is always present
	// (we discover and call device tools); "vision" only when a callback URL is
	// configured. The server dictates audio params (it does not negotiate the
	// device's offer — PROTOCOL_V2 §3.2): fixed 16k mic / 24k TTS hardware.
	features := []string{"tools"}
	if s.cfg.VisionURL != "" {
		features = append(features, "vision")
	}

	resp := protov2.ServerHello{
		Type:   "hello",
		ID:     hello.ID, // echo the request id (PROTOCOL_V2 §3.2)
		Result: "ok",
		Server: &protov2.ServerInfo{Name: serverName, Version: serverVersion},
		Audio: &protov2.AudioConfig{
			In:  protov2.AudioStream{Rate: mic.SampleRate, FrameMS: mic.FrameDuration},
			Out: protov2.AudioStream{Rate: tts.SampleRate, FrameMS: tts.FrameDuration},
		},
		Features:    features,
		Time:        s.clockSync(),
		FlowControl: &protov2.FlowControl{AudioCreditInitial: s.audioCredit()},
	}
	// vision_url is advertised here (not in discovery) so it can move freely.
	if s.cfg.VisionURL != "" {
		resp.VisionURL = s.cfg.VisionURL
	}
	return writeJSON(hsCtx, s.conn, resp)
}

// Server identity advertised in the v2 hello.
const (
	serverName    = "stackchan-server"
	serverVersion = "0.1.0"
)

// clockSync builds the device clock-sync payload for the v2 hello (§3.2),
// folding v1's separate OTA server_time round-trip into the handshake. The
// timezone is cfg.TimeLocation (the container runs in UTC, so set it to the
// user's zone for a correct local-time offset); nil falls back to UTC, which
// also keeps the value machine-independent in tests.
func (s *Session) clockSync() *protov2.TimeSync {
	loc := s.cfg.TimeLocation
	if loc == nil {
		loc = time.UTC
	}
	now := time.Now()
	_, offsetSec := now.In(loc).Zone()
	return &protov2.TimeSync{
		UnixMS:      now.UnixMilli(),
		TZOffsetMin: offsetSec / 60,
	}
}

// audioCredit is the v2 initial audio flow-control budget, falling back to the
// default when unset.
func (s *Session) audioCredit() int {
	if s.cfg.AudioCreditInitial > 0 {
		return s.cfg.AudioCreditInitial
	}
	return defaultAudioCredit
}

// tailPadFrames is the number of trailing silent frames appended to each v2
// reply, derived from TTSTailPadMS and the TTS frame duration. 0 → no pad.
func (s *Session) tailPadFrames() int {
	frameMS := s.cfg.TTSAudio.FrameDuration
	if s.cfg.TTSTailPadMS <= 0 || frameMS <= 0 {
		return 0
	}
	return s.cfg.TTSTailPadMS / frameMS
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
			s.onAudioFrame(ctx, data)
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
	s.prerollOpen = false
	s.armEndpointer()
}

// armEndpointer builds (auto mode) or clears (manual mode) the server-side
// endpoint detector for the current mode, without touching the mic buffer.
// Split out of armListen so the wake→listen_start pre-roll path can attach the
// detector at listen_start while keeping the already-buffered pre-roll.
func (s *Session) armEndpointer() {
	if s.autoStop {
		cfg := s.cfg.VAD
		cfg.FrameMS = s.cfg.MicAudio.FrameDuration
		s.ep = newEndpointer(cfg)
	} else {
		s.ep = nil
	}
}

func (s *Session) onAudioFrame(ctx context.Context, opusBytes []byte) {
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
		s.fireTurn(ctx, "buffer-full")
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
			s.fireTurn(ctx, "server-vad")
		}
	}
}

func (s *Session) dispatchText(ctx context.Context, data []byte) {
	ev, err := s.dec.Decode(data)
	if err != nil {
		s.log.Warn("decode failed", "err", err, "raw", string(data))
		// A malformed frame is a protocol violation (PROTOCOL_V2 §9.4): report
		// it and keep the session alive. No-op under v1 (no error message type).
		s.sendError(ctx, "PROTOCOL_VIOLATION", err.Error(), 0)
		return
	}
	switch e := ev.(type) {
	case evDupHello:
		s.log.Warn("duplicate hello received")
	case evListenStart:
		s.log.Info("listen start", "mode", e.Mode)
		// Device-driven listen start supersedes any pending re-arm. auto-stop
		// mode (or unspecified, which the firmware uses as the AEC-off default)
		// means the device streams continuously and never sends listen:stop —
		// we endpoint server-side. Manual mode sends its own stop, so skip the
		// detector.
		s.autoStop = e.Mode == "auto" || e.Mode == ""
		s.rearm.Store(false)
		if s.prerollOpen {
			// The window was already opened by a wake pre-roll (§4.2). Keep the
			// buffered pre-roll and just attach the now-known mode's detector;
			// re-arming here would discard the pre-roll we just captured.
			s.prerollOpen = false
			s.armEndpointer()
		} else {
			s.armListen()
		}
	case evListenStop:
		// Device-initiated stop (manual mode / device VAD). Fire the turn.
		s.log.Info("listen stop")
		s.fireTurn(ctx, "device-stop")
	case evWake:
		// Wake word fired (PROTOCOL_V2 §4.2). With the wake_word_audio feature,
		// a wake from idle opens the turn's audio window early so the device's
		// buffered pre-roll (mic captured before the wake fired) is accepted as
		// turn audio; the following listen_start attaches the mode's detector.
		// We buffer without endpointing here (mode unknown; pre-roll must not
		// self-endpoint). Without the feature, or mid-window (a barge-in wake,
		// handled by the following abort), wake is informational and the window
		// opens at listen_start as before.
		s.log.Info("wake", "phrase", e.Phrase, "preroll", s.wakeWordAudio && !s.listening)
		if s.wakeWordAudio && !s.listening {
			s.micBuf = s.micBuf[:0]
			s.micDropped = false
			s.listening = true
			s.prerollOpen = true
			s.ep = nil
			s.rearm.Store(false)
		}
	case evAbort:
		// Barge-in: cancel the in-flight turn. The TTS sink emits audio_cancel
		// (v2) / tts:stop (v1) as it unwinds; a fresh turn follows.
		s.log.Info("abort", "reason", e.Reason)
		s.cancelTurn()
	case evToolResponse:
		// v2 first-class tool response (whole frame). Route to the tool port.
		if s.toolPort != nil {
			s.toolPort.HandleIncoming(e.Raw)
		} else {
			s.log.Debug("tool message but no tool port", "bytes", len(e.Raw))
		}
	case evTelemetry:
		s.handleTelemetry(ctx, e)
	case evAudioCredit:
		// v2 audio flow control: route the refill to the in-flight TTS stream.
		// Only the v2 sink implements creditSink; v1 never produces this event.
		if cs, ok := s.out.(creditSink); ok {
			cs.AddCredit(e.Frames)
		}
	case evGoodbye:
		// Advisory only; the WS close frame does the real work (PROTOCOL_V2 §4.10).
		s.log.Info("goodbye", "reason", e.Reason)
	case evError:
		s.log.Warn("device error", "code", e.Code, "message", e.Message)
	case evUnknown:
		s.log.Info("unknown message type", "type", e.Type)
	}
}

// handleTelemetry reacts to an ambient device-perception event (PROTOCOL_V2
// §4.8). Every event is logged; recognized ones may drive device feedback. The
// reference policy wires battery_low → a full-screen "charge me" alert (§4.6)
// when the protocol supports alerts (v2; v1 is log-only). Unknown events are
// logged and ignored — forward-compatible, never a protocol error. Richer
// wiring (feeding events into the LLM's ambient context) is future work.
func (s *Session) handleTelemetry(ctx context.Context, e evTelemetry) {
	s.log.Info("telemetry", "event", e.Name, "data", string(e.Data))
	switch e.Name {
	case "battery_low":
		as, ok := s.out.(alertSink)
		if !ok {
			return // protocol has no alert channel (v1): log-only above
		}
		msg := "Please charge me"
		if pct := batteryPercent(e.Data); pct >= 0 {
			msg = fmt.Sprintf("Battery at %d%% — please charge me", pct)
		}
		if err := as.SendAlert(ctx, "Battery low", msg, "sad", "vibration"); err != nil {
			s.log.Warn("send battery alert", "err", err)
		}
	}
}

// batteryPercent extracts an integer percent from a battery_low telemetry
// payload ({"percent": N}), or -1 if absent/unparseable.
func batteryPercent(data json.RawMessage) int {
	if len(data) == 0 {
		return -1
	}
	var d struct {
		Percent *int `json:"percent"`
	}
	if err := json.Unmarshal(data, &d); err != nil || d.Percent == nil {
		return -1
	}
	return *d.Percent
}

// sendError reports a protocol error to the device when the active protocol
// supports one (PROTOCOL_V2 §9); under v1 it is a no-op. It is suppressed when
// ctx is already cancelled, so a deliberate barge-in (which surfaces as
// "context canceled" deep in the turn) is never misreported as a failure.
func (s *Session) sendError(ctx context.Context, code, message string, refID int) {
	if ctx.Err() != nil {
		return
	}
	if es, ok := s.out.(errorSink); ok {
		es.SendError(ctx, code, message, refID)
	}
}

// classifyTurnError maps a turn-pipeline error to a v2 error code by inspecting
// the wrapper prefixes streamReply adds ("llm stream:", "tts ...:"). A coarse
// mapping is enough for the device to log/degrade; INTERNAL is the catch-all.
func classifyTurnError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "llm"):
		return "LLM_FAILED"
	case strings.Contains(msg, "tts") || strings.Contains(msg, "kokoro") || strings.Contains(msg, "speak"):
		return "TTS_FAILED"
	default:
		return "INTERNAL"
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
