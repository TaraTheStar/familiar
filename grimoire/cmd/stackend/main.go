// SPDX-License-Identifier: AGPL-3.0-or-later

// Command stackend is the local-first voice-loop server for StackChan.
//
// Milestone 1 scope:
//
//   - HTTP /ota endpoint: returns WebSocket URL + server time
//   - WebSocket /xiaozhi/v1/: accepts device connections, performs hello
//     handshake, logs subsequent messages
//
// No ASR/LLM/TTS yet — those come in subsequent milestones.
package main

import (
	"context"
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
	"github.com/TaraTheStar/familiar/grimoire/internal/ota"
	"github.com/TaraTheStar/familiar/grimoire/internal/protocol"
	"github.com/TaraTheStar/familiar/grimoire/internal/session"
	"github.com/TaraTheStar/familiar/grimoire/internal/tts"
	"github.com/TaraTheStar/familiar/grimoire/internal/vision"
)

func main() {
	var (
		addr           = flag.String("addr", ":9098", "listen address (host:port)")
		wsURL          = flag.String("ws-url", "", "WebSocket URL to advertise to devices (e.g. ws://192.0.2.10:9098/xiaozhi/)")
		fwVer          = flag.String("firmware-version", "1.3.1", "firmware version echoed in /ota (set to device's current to suppress OTA)")
		logLevel       = flag.String("log-level", "info", "debug|info|warn|error")
		kokoroURL      = flag.String("kokoro-url", "", "Kokoro fastapi base URL (e.g. http://kokoro:8880); empty disables TTS")
		kokoroVoice    = flag.String("kokoro-voice", "af_heart", "Kokoro voice name")
		kokoroSpeed    = flag.Float64("kokoro-speed", 0.85, "Kokoro playback speed multiplier")
		kokoroLang     = flag.String("kokoro-lang", "a", "Kokoro lang_code (a=American English)")
		whisperModel   = flag.String("whisper-model", "", "Path to ggml whisper model (.bin); empty disables ASR (use -hardcoded-reply instead)")
		whisperThreads = flag.Int("whisper-threads", 0, "Whisper inference thread count (0=NumCPU)")
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

		// Protocol v2 audio flow control: the server→device send budget, in
		// 60ms Opus frames, advertised in the v2 hello. Tunable so the credit
		// window can be matched to a real device's decoder buffer without a
		// rebuild (v1 ignores it). 0 = built-in default (40 frames ≈ 2.4s).
		audioCreditInitial = flag.Int("v2-audio-credit", 0, "Protocol v2: initial server→device audio credit in 60ms frames (0=default 40)")

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
		logger.Error("-ws-url is required (e.g. ws://192.0.2.10:9098/xiaozhi/)")
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
	// The flashed firmware (sdkconfig: CONFIG_OTA_URL) dials /xiaozhi/ota/.
	// Mount /ota/ too so either discovery path works without a reflash.
	mux.HandleFunc("/xiaozhi/ota/", otaHandler)
	mux.HandleFunc("/ota/", otaHandler)
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

	sessionHandler := session.Handler(session.Config{
		TTSAudio:         protocol.AudioParams{SampleRate: 24000, FrameDuration: 60},
		HandshakeTimeout: 8 * time.Second,
		ReadIdleTimeout:  120 * time.Second,
		Kokoro:           kokoro,
		ASR:              whisper,
		LLM:              llmClient,
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
	// The session is dispatched by the Protocol-Version header, not the path, so
	// both v1 and v2 are served on either mount. /xiaozhi/ is the version-neutral
	// canonical path (advertised to v2 devices); /xiaozhi/v1/ stays mounted for
	// devices that cached it.
	mux.HandleFunc("/xiaozhi/", sessionHandler)
	mux.HandleFunc("/xiaozhi/v1/", sessionHandler)

	// Vision callback: device POSTs camera captures here when the LLM
	// calls self.camera.take_photo. Requires a multimodal LLM (we reuse
	// the same -llm-url; gemma4 with mmproj works fine).
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
