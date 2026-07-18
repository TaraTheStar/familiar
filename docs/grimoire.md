---
title: "Deep dive: grimoire"
weight: 5
toc: true
---

# Grimoire — the mind

A deep dive into the server half of familiar: a single Go binary (`stackend`)
that turns your LAN box into the familiar's brain. It hears with whisper.cpp,
thinks with any OpenAI-compatible LLM, and speaks with Kokoro TTS.

If you just want it running, the [Quickstart](QUICKSTART.md) covers that in
four commands. This page is for understanding — and tuning — what's inside.

## The shape of it

```
              WebSocket /grimoire/            HTTP
 poppet ◄──────────────────────────► stackend ─────► llama-swap / vLLM (LLM)
 (device)     Opus audio + JSON          │
                                         ├──── in-process (cgo): whisper.cpp (ASR)
                                         └──── HTTP: Kokoro sidecar (TTS, 24 kHz PCM)
```

One process, one WebSocket per device, and **the connection is the session** —
protocol v2 has no session IDs. Everything ships as a single container plus a
Kokoro sidecar (`compose.yaml`), listening on port **9099**.

### Package tour

| Path | What it is |
|---|---|
| `cmd/stackend/` | the server binary — flags, HTTP mux, graceful shutdown |
| `cmd/v2client/` | reference protocol client; smoke-test a full turn without a robot |
| `internal/session/` | the heart: per-connection state machine and voice loop |
| `internal/protov2/` | v2 wire format — message types, decoder, fuzz tests |
| `internal/audio/` | Opus encode/decode (libopus via cgo), framing, 24→16 kHz resample |
| `internal/asr/` | whisper.cpp via cgo + the whisper→JSON-log bridge |
| `internal/llm/` | streaming OpenAI-compatible client (SSE, tools, images) |
| `internal/tts/` | Kokoro client + sentence chunking |
| `internal/mcptools/` | external MCP adapter — plugs standard MCP servers into the LLM |
| `internal/vision/` | `/vision` endpoint: camera captures → multimodal LLM |
| `internal/ota/` | `/discover` and OTA discovery endpoints |

## Anatomy of a turn

What actually happens between "Hey Artemis, what time is it?" and the answer:

1. **Wake.** The device detects the wake word on-device and opens the audio
   window, including a short **pre-roll** — so words spoken right on the heels
   of the wake phrase aren't lost.
2. **Listening.** The device streams 16 kHz Opus mic audio continuously. In the
   default auto-stop mode the *server* decides when you stopped talking: an
   adaptive energy VAD (`internal/session/vad.go`) tracks the noise floor and
   fires after ~240 ms of speech followed by ~800 ms of silence. (Frames louder
   than the beep ceiling are ignored, so the wake chime can't trigger it.)
3. **Beep trim + ASR.** The wake-ack chime bleeds into the mic (its echo isn't
   cancelled), so the first ~600 ms is scanned and the beep stripped —
   otherwise whisper hears "(gasp)". Then whisper.cpp transcribes in-process.
   Whisper's stage directions (`[BLANK_AUDIO]`, `(wind)`, `♪`) are filtered out.
4. **Transcript to screen.** The final transcript goes to the device as a
   caption; with `-asr-streaming` on, incremental partials appear *while you
   speak* (each partial is a real whisper re-inference — opt-in for that reason).
5. **Think.** Your words join the dialogue history (capped at 48 messages) and
   stream through the LLM. Tool calls the model makes are dispatched — up to 6
   rounds of call → result → continue.
6. **Speak.** Each complete sentence of the reply goes to Kokoro as it arrives,
   comes back as 24 kHz PCM, is resampled to 16 kHz server-side, and streams to
   the device as Opus — with cumulative captions so the text tracks the voice.
   The whole reply is one speaking session, so multi-sentence answers never
   get clipped between sentences.

### Flow control: credits, not clocks

The ESP32 has a fixed 40-packet audio buffer. Rather than pacing TTS by
wall-clock, every Opus frame the server sends spends a **credit**; the device
grants credits back as it drains its buffer. When credits hit zero the server
blocks. The result: the device buffer physically cannot overrun, and there's
no drift math anywhere. (Protocol details: [Protocol v2 §5](PROTOCOL_V2.md).)

### Half-duplex, for now

The shipped release is half-duplex: wait for the reply to finish before
speaking again. Barge-in (interrupting mid-reply) is designed into the
protocol — `abort` handling, `audio_cancel`, hello renegotiation — but needs
server-side echo cancellation, and is deferred to v2.1.

## Tools: three tiers

When the LLM decides to *do* something instead of just talking, the tool call
is routed by where the tool lives:

- **Server built-ins** (`internal/session/tools_local.go`) — currently one:
  `get_current_time`, phrased for speech ("Monday, January 2… 3:04 PM") in
  your `-timezone`. Never touches the device.
- **Device tools** — the robot's own catalog, discovered over the wire at
  connect (or announced inline in the hello). This is how "be a cat" works:
  the LLM calls `self.avatar.set_familiar`, and the firmware swaps the face.
  Head movement, LEDs, reminders, and `set_state` (sleep) live here too.
  Sleep is special-cased: the server defers it until *after* the farewell is
  spoken, then clears the dialogue so a re-wake starts fresh.
- **External MCP servers** (`internal/mcptools/`) — point `-mcp-config` at a
  JSON list of standard MCP servers (stdio or HTTP) and their tools join the
  LLM's catalog namespaced as `mcp__<server>__<tool>`. The canonical use is a
  `fetch` server so the familiar can look things up (weather, news). These are
  **LLM-facing only** — dispatched on the server; the device never sees them.
  A server that fails to launch is logged and skipped, never fatal.

## Configuration

Deployment config flows `.env` → `compose.yaml` → flags. The interesting knobs:

| Flag | Default | What it does |
|---|---|---|
| `-ws-url` | *(required)* | the WebSocket URL advertised to devices via `/discover` |
| `-whisper-model` | — | path to a ggml model; empty disables ASR |
| `-llm-url` / `-llm-model` / `-llm-api-key` | — | any OpenAI-compatible endpoint |
| `-llm-max-tokens` | 200 | caps runaway replies (and therefore runaway TTS) |
| `-system-prompt-file` | — | `persona.txt` — your familiar's personality |
| `-kokoro-voice` / `-kokoro-speed` | `af_heart` / 0.85 | the voice |
| `-tts-tail-pad-ms` | 400 | trailing silence so the last word never clips |
| `-asr-streaming` | off | live partial transcripts while you speak |
| `-vad-*` | see `--help` | endpointing: silence window, speech threshold, beep ceiling |
| `-wake-gate` | off | second-stage wake-word check (below) |
| `-mcp-config` | — | external MCP servers for the LLM |
| `-timezone` | UTC | for `get_current_time` |
| `-vision-url` | — | enables the camera→multimodal-LLM path |

`persona.txt` deserves a special mention: it's the system prompt, and it's
where your familiar's character lives. The shipped one keeps replies to a
couple of short sentences (this is a *spoken* companion — nobody wants a
read-aloud essay), bans markdown, and teaches it when to use its tools.

## Observability

Everything logs as structured JSON on stderr (`slog`), one line per event,
tagged by component — sessions, turns, tool calls, per-frame VAD state at
debug level. Two pieces are worth knowing about:

- **The whisper log bridge** (`internal/asr/log.go`): whisper.cpp and ggml
  natively write raw text to stderr, which would corrupt the JSON stream. A C
  callback reroutes every line into `slog` — so ASR internals show up as
  ordinary log events, and a default `info` deployment stays quiet.
- **The wake gate** (`-wake-gate`, `internal/session/wakegate.go`): a
  second-stage defense against false wakes. The on-device wake model can
  confuse similar-sounding phrases; since the wake phrase leaks into the first
  turn's audio, whisper's transcript acts as a second, sharper classifier. It
  fails open — only the first post-wake turn is checked, and only bare
  wake-word-lookalikes with no real content are rejected.

HTTP surface: `/discover` (device bootstrap), `/grimoire/` (the WebSocket),
`/healthz` (liveness), `/vision` (camera callback — unauthenticated by design,
keep it LAN-only), plus legacy OTA-shaped discovery at `/grimoire/ota/`.

## Running it well

- **CPU is fine.** whisper `base.en` + Kokoro-CPU serve a single device
  comfortably on a modest box; ASR is serialized through one shared model.
- **Audio is 16 kHz mono end-to-end** on the wire (60 ms Opus frames, both
  directions). Kokoro's native 24 kHz is resampled server-side on purpose:
  the device plays straight from its Opus decoder with no on-device resample,
  which keeps the audio path cheap enough for wake-word processing to coexist.
- **Test without the robot.** `cmd/v2client` speaks the exact same protocol as
  the firmware; point it at your server with any 16 kHz WAV and you exercise
  ASR → LLM → TTS end-to-end from your desk (see the
  [Quickstart](QUICKSTART.md#2-server-grimoire)).
- **License note:** grimoire is AGPL-3.0-or-later — if you run a modified
  grimoire as a service for others, §13 obliges you to offer them your source.
