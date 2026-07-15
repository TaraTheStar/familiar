// SPDX-License-Identifier: AGPL-3.0-or-later

// Command stackend is the local-first voice-loop server for StackChan.
//
// Milestone 1 scope:
//
//   - HTTP /ota endpoint: returns WebSocket URL + server time
//   - WebSocket /grimoire/: accepts device connections, performs the hello
//     handshake, runs the voice loop (v2-only since the v1 removal — WS3)
//
// No ASR/LLM/TTS yet — those come in subsequent milestones.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // embed the IANA tz database so LoadLocation works in the slim runtime image

	"github.com/TaraTheStar/familiar/grimoire/internal/asr"
	"github.com/TaraTheStar/familiar/grimoire/internal/llm"
	"github.com/TaraTheStar/familiar/grimoire/internal/mcptools"
	"github.com/TaraTheStar/familiar/grimoire/internal/ota"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/session"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
	"github.com/TaraTheStar/familiar/grimoire/internal/vision"
)

func main() {
	var (
		addr           = flag.String("addr", ":9098", "listen address (host:port)")
		wsURL          = flag.String("ws-url", "", "WebSocket URL to advertise to devices (e.g. ws://192.0.2.10:9098/grimoire/)")
		fwVer          = flag.String("firmware-version", "1.3.1", "firmware version echoed in /ota (set to device's current to suppress OTA)")
		logLevel       = flag.String("log-level", "info", "debug|info|warn|error")
		kokoroURL      = flag.String("kokoro-url", "", "Kokoro fastapi base URL (e.g. http://kokoro:8880); empty disables TTS")
		kokoroVoice    = flag.String("kokoro-voice", "af_heart", "Kokoro voice name")
		kokoroSpeed    = flag.Float64("kokoro-speed", 0.85, "Kokoro playback speed multiplier")
		kokoroLang     = flag.String("kokoro-lang", "a", "Kokoro lang_code (a=American English)")
		whisperModel   = flag.String("whisper-model", "", "Path to ggml whisper model (.bin); empty disables ASR (use -hardcoded-reply instead)")
		whisperThreads = flag.Int("whisper-threads", 0, "Whisper inference thread count (0=NumCPU)")
		asrStreaming   = flag.Bool("asr-streaming", false, "Emit incremental transcript{final:false} partials while the user speaks (re-runs whisper on the growing buffer; off=single final transcript per turn)")
		llmURL         = flag.String("llm-url", "", "OpenAI-compatible LLM endpoint (e.g. http://192.0.2.20:8080); empty disables LLM")
		llmModel       = flag.String("llm-model", "", "LLM model name as registered with llama-swap (e.g. gemma4-26B-A4B)")
		llmAPIKey      = flag.String("llm-api-key", "", "Bearer token for the LLM endpoint")
		llmMaxTokens   = flag.Int("llm-max-tokens", 200, "Hard cap on LLM response tokens (prevents runaway TTS)")
		systemPrompt   = flag.String("system-prompt", "", "Persona / system prompt prepended to every LLM call (overrides -system-prompt-file)")
		systemPromptF  = flag.String("system-prompt-file", "", "Path to a file containing the persona / system prompt")
		visionURL      = flag.String("vision-url", "", "URL the device will POST camera JPEGs to (e.g. http://192.0.2.10:9098/vision); empty disables the camera tool")
		hardcodedReply = flag.String("hardcoded-reply", "", "DEBUG: when set, BYPASSES ASR+LLM and speaks this on every listen:stop")

		// Server-side VAD (auto-stop mode endpointing). Tunable without a
		// rebuild so thresholds can be dialed in against a real device.
		vadMinSpeechMS  = flag.Int("vad-min-speech-ms", 0, "VAD: min cumulative speech before a turn can endpoint (0=default 240)")
		vadEndSilenceMS = flag.Int("vad-end-silence-ms", 0, "VAD: trailing silence that ends a turn (0=default 800)")
		vadSpeechFactor = flag.Float64("vad-speech-factor", 0, "VAD: speech threshold = noise_floor * factor (0=default 2.5)")
		vadMinThreshold = flag.Float64("vad-min-threshold", 0, "VAD: absolute floor on the speech threshold, mean-abs amplitude (0=default 180)")
		vadBeepCeiling  = flag.Float64("vad-beep-ceiling", 0, "VAD: ignore frames louder than this (the wake beep), so it can't arm the endpoint (0=default 2500)")

		debugDumpDir = flag.String("debug-dump-dir", "", "DEBUG: write each captured turn's PCM as a 16kHz mono WAV here for playback")

		timezone = flag.String("timezone", "UTC", "IANA timezone the get_current_time tool reports in (e.g. America/New_York)")

		mcpConfig = flag.String("mcp-config", "", "Path to a JSON config of external MCP servers to bridge into the LLM tool catalog (see mcp.example.json); empty disables the adapter")

		// Protocol v2 audio flow control: the server→device send budget, in
		// 60ms Opus frames, advertised in the v2 hello. Tunable so the credit
		// window can be matched to a real device's decoder buffer without a
		// rebuild (v1 ignores it). 0 = built-in default (40 frames ≈ 2.4s).
		audioCreditInitial = flag.Int("v2-audio-credit", 0, "Protocol v2: initial server→device audio credit in 60ms frames (0=default 40)")

		// TTS output rate dictated to the device. 16000 matches the StackChan's
		// native (AEC-locked) 16kHz output, so the device plays Opus straight
		// from the decoder with no on-device resample — the server downsamples
		// Kokoro's fixed 24kHz instead. 24000 reverts to Kokoro-native (the
		// device then resamples 24k→16k itself, which competes with wakenet/AEC
		// during playback). Tunable for A/B without a rebuild. Must be an Opus
		// rate (8000/12000/16000/24000).
		ttsSampleRate = flag.Int("tts-sample-rate", 16000, "TTS sample rate dictated to the device in Hz (16000=device-native, no on-device resample; 24000=Kokoro-native)")

		// Trailing silence appended to each v2 reply. v2 has no wall-clock drain,
		// so the device can exit speaking just before its buffer empties and clip
		// the last word; this pad makes that cut land in silence. 400ms mirrors
		// v1's ttsLead; 0 disables.
		ttsTailPadMS = flag.Int("tts-tail-pad-ms", 400, "Protocol v2: trailing silence (ms) appended to each reply so the device doesn't clip the last word (0=off)")

		// Leading wake-beep removal (the chime bleeds into the mic with no AEC).
		beepTrimMaxMS     = flag.Int("asr-beep-trim-ms", 600, "ASR: scan this many leading ms for the wake beep and trim it before transcription (0 disables)")
		beepTrimThreshold = flag.Float64("asr-beep-threshold", 0, "ASR: mean-abs amplitude separating the beep from speech (0=default 2000)")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(*logLevel),
	}))
	slog.SetDefault(logger)

	if *wsURL == "" {
		logger.Error("-ws-url is required (e.g. ws://192.0.2.10:9098/grimoire/)")
		os.Exit(2)
	}

	mux := http.NewServeMux()
	otaHandler := ota.Handler(ota.Config{
		WebSocketURL:    *wsURL,
		FirmwareVersion: *fwVer,
		NowMillis: func() (int64, int) {
			now := time.Now()
			_, off := now.Zone()
			return now.UnixMilli(), off / 60
		},
	}, logger.With("component", "ota"))
	// Brand-neutral OTA aliases (no /<brand>/ prefix), kept so a device flashed
	// with a bare CONFIG_OTA_URL=.../ota works regardless of brand.
	mux.HandleFunc("/ota/", otaHandler)
	mux.HandleFunc("/ota", otaHandler)
	// Protocol v2 discovery (PROTOCOL_V2 §2.1): the lean {ws_url, firmware?}
	// shape. v1 firmware keeps using /ota; v2 firmware uses /discover.
	mux.HandleFunc("/discover", ota.DiscoverHandler(ota.Config{
		WebSocketURL:    *wsURL,
		FirmwareVersion: *fwVer,
	}, logger.With("component", "discover")))

	var kokoro *tts.KokoroClient
	if *kokoroURL != "" {
		kokoro = &tts.KokoroClient{
			BaseURL:  *kokoroURL,
			Voice:    *kokoroVoice,
			Speed:    *kokoroSpeed,
			LangCode: *kokoroLang,
		}
		logger.Info("TTS configured", "kokoro_url", *kokoroURL, "voice", *kokoroVoice)
	} else {
		logger.Warn("Kokoro URL not set; TTS will fail. Set -kokoro-url or -hardcoded-reply.")
	}

	var whisper session.ASR
	if *whisperModel != "" {
		w, err := asr.New(asr.Config{ModelPath: *whisperModel, Threads: *whisperThreads})
		if err != nil {
			logger.Error("whisper init failed", "err", err)
			os.Exit(1)
		}
		whisper = w
		logger.Info("ASR configured", "whisper_model", *whisperModel,
			"whisper_version", asr.Version())
	}

	var llmClient *llm.Client
	if *llmURL != "" {
		llmClient = &llm.Client{
			BaseURL:   *llmURL,
			Model:     *llmModel,
			APIKey:    *llmAPIKey,
			MaxTokens: *llmMaxTokens,
		}
		logger.Info("LLM configured", "llm_url", *llmURL, "model", *llmModel)
	}

	// Resolve system prompt: explicit flag wins, else file, else empty.
	sysPrompt := *systemPrompt
	if sysPrompt == "" && *systemPromptF != "" {
		b, err := os.ReadFile(*systemPromptF)
		if err != nil {
			logger.Error("read system-prompt-file failed", "path", *systemPromptF, "err", err)
			os.Exit(1)
		}
		sysPrompt = string(b)
		logger.Info("system prompt loaded", "path", *systemPromptF, "len", len(sysPrompt))
	}

	timeLocation, err := time.LoadLocation(*timezone)
	if err != nil {
		logger.Error("invalid -timezone", "value", *timezone, "err", err)
		os.Exit(2)
	}
	logger.Info("timezone configured", "tz", *timezone)

	// External MCP tool adapter: bridge configured standard-MCP servers into the
	// LLM tool catalog (PROTOCOL_V2 §6.5). Connects at startup; a server that
	// fails is logged and skipped, never fatal. nil when no config is given.
	var serverTools session.ToolProvider
	if *mcpConfig != "" {
		mcpCfg, err := loadMCPConfig(*mcpConfig)
		if err != nil {
			logger.Error("read -mcp-config failed", "path", *mcpConfig, "err", err)
			os.Exit(2)
		}
		mgr := mcptools.New(context.Background(), mcpCfg, logger.With("component", "mcp"))
		defer mgr.Close()
		serverTools = mgr
	}

	sessionHandler := session.Handler(session.Config{
		TTSAudio:         protocol.AudioParams{SampleRate: *ttsSampleRate, FrameDuration: 60},
		TTSTailPadMS:     *ttsTailPadMS,
		HandshakeTimeout: 8 * time.Second,
		ReadIdleTimeout:  120 * time.Second,
		Kokoro:           kokoro,
		ASR:              whisper,
		ASRStreaming:     *asrStreaming,
		LLM:              llmClient,
		ServerTools:      serverTools,
		SystemPrompt:     sysPrompt,
		VisionURL:        *visionURL,
		HardcodedReply:   *hardcodedReply,
		DebugDumpDir:     *debugDumpDir,
		VAD: session.EndpointConfig{
			MinSpeechMS:  *vadMinSpeechMS,
			EndSilenceMS: *vadEndSilenceMS,
			SpeechFactor: *vadSpeechFactor,
			MinThreshold: *vadMinThreshold,
			BeepCeiling:  *vadBeepCeiling,
		},
		BeepTrim: session.BeepTrimConfig{
			MaxScanMS: *beepTrimMaxMS,
			Threshold: *beepTrimThreshold,
		},
		AudioCreditInitial: *audioCreditInitial,
		TimeLocation:       timeLocation,
		Logger:             logger.With("component", "session"),
	})
	// Mount the OTA + session routes under the /grimoire/ brand. The legacy
	// /xiaozhi/ mount was dropped with the v1 protocol removal (WS3): the device
	// is reflashed to dial /grimoire/ota/ before it comes back online, so no
	// compatibility alias is needed. Brand-neutral /ota, /ota/, /discover are
	// registered above.
	mountBrand(mux, "grimoire", otaHandler, sessionHandler)

	// Vision callback: device POSTs camera captures here when the LLM
	// calls self.camera.take_photo. Requires a multimodal LLM (we reuse
	// the same -llm-url; gemma4 with mmproj works fine).
	//
	// NOTE: /vision is unauthenticated — each POST burns a multimodal LLM
	// call, so keep the port LAN-only (the compose deployment does). Auth
	// would need the device to send a token with its capture, which the wire
	// doesn't carry today.
	if *visionURL != "" && llmClient != nil {
		mux.HandleFunc("/vision", vision.Handler(vision.Config{
			LLM:    llmClient,
			Logger: logger.With("component", "vision"),
		}))
		logger.Info("Vision endpoint mounted at /vision", "advertised_url", *visionURL)
	}

	// Basic liveness probe; convenient for `curl host:9098/healthz`.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("stackend listening",
			"addr", *addr,
			"ws_url", *wsURL,
			"firmware_version", *fwVer,
		)
		serverErr <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining...")
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server crashed", "err", err)
			os.Exit(1)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown error", "err", err)
	}
	logger.Info("stopped")
}

// mountBrand registers the OTA + WebSocket-session routes for the given brand
// prefix (e.g. "grimoire") on mux:
//
//   - /<brand>/ota/  + bare /<brand>/ota : the flashed CONFIG_OTA_URL target.
//     The bare (no-slash) form is registered explicitly because a device flashed
//     with ".../<brand>/ota" (no slash) would otherwise hit ServeMux's subtree
//     redirect to ".../<brand>/ota/" — a 307 the ESP OTA client won't follow.
//     The exact patterns out-rank the "/<brand>/" session subtree, so the bare
//     path returns the OTA body directly instead of redirecting.
//   - /<brand>/ : the WebSocket session (v2-only since WS3; the upgrade is
//     gated on the Protocol-Version header in session.Handler).
//
// The brand-neutral /ota, /ota/, and /discover routes are registered once by the
// caller (not per brand).
func mountBrand(mux *http.ServeMux, brand string, otaHandler, sessionHandler http.HandlerFunc) {
	mux.HandleFunc("/"+brand+"/ota/", otaHandler)
	mux.HandleFunc("/"+brand+"/ota", otaHandler)
	mux.HandleFunc("/"+brand+"/", sessionHandler)
}

// loadMCPConfig reads and parses the -mcp-config JSON file.
func loadMCPConfig(path string) (mcptools.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return mcptools.Config{}, err
	}
	var cfg mcptools.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return mcptools.Config{}, err
	}
	return cfg, nil
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
