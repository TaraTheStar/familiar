# grimoire

The backend half of [familiar](../README.md) — the local-first voice-loop server
for the StackChan ESP32 robot ([poppet](../poppet/)).

Replaces [xiaozhi-esp32-server](https://github.com/xinnan-tech/xiaozhi-esp32-server) for
self-hosted deployments. Implements the v1 wire protocol that the
[poppet firmware](../poppet/) speaks.

## Status

Live. The full voice loop (mic → ASR → LLM → TTS → speaker) is verified on hardware.

## What it does

Mediates between a StackChan robot and a stack of locally-hosted AI services:

```
device ──WS──> stackend ──HTTP──> llama-swap   (LLM)
                       ├─────────> whisper.cpp  (ASR, in-process via cgo)
                       └─────────> Kokoro       (TTS)
```

See [../docs/PROTOCOL_V1.md](../docs/PROTOCOL_V1.md) for the live wire protocol
this implements, and [../docs/PROTOCOL_V2.md](../docs/PROTOCOL_V2.md) for the
future-direction redesign (still a draft).

## Layout

```
cmd/stackend/        # entry point
internal/
  protocol/          # v1 wire format — JSON message types + binary framing
  session/           # per-connection state machine
  audio/             # Opus encode/decode, frame muxing
  asr/               # whisper.cpp ASR (cgo, model bundled in image)
  llm/               # OpenAI-compatible client (talks to llama-swap)
  tts/               # Kokoro client + streaming sentence chunking
  mcp/               # MCP-over-WS client (server-side, calls into device tools)
  ota/               # /ota/ HTTP endpoint
```

## Build

First-time setup (clones whisper.cpp at the pinned tag and builds its static libs):

```
git submodule update --init --recursive
make whisper-build
make build
```

Subsequent builds:

```
make build      # builds stackend, reusing whisper.cpp libs
make test       # fast unit tests (uses bundled <1MB mock whisper model)
make tiny-model # one-time download of real ggml-tiny.en.bin (~75MB)
make test-real-asr  # transcribes the JFK sample with the real tiny model
```

System requirements: Go 1.26+, cmake, g++, libopus-dev, libopusfile-dev,
pkg-config. On Debian/Ubuntu:

```
apt install golang cmake build-essential libopus-dev libopusfile-dev pkg-config
```

## License

Copyright (C) 2026 TaraTheStar.

grimoire is free software: you can redistribute it and/or modify it under the
terms of the **GNU Affero General Public License** as published by the Free
Software Foundation, either **version 3 of the License, or (at your option) any
later version**. See [LICENSE](LICENSE).

It is distributed in the hope that it will be useful, but WITHOUT ANY WARRANTY;
without even the implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR
PURPOSE. Because the AGPL covers network use (§13), anyone who runs a modified
grimoire as a network service must offer its users the corresponding source.

This is the backend half only. The rest of the [familiar](../README.md) repo —
including the [poppet](../poppet) firmware — is MIT-licensed; grimoire and poppet
are separate programs communicating over a socket, so grimoire's copyleft does
not extend to poppet. Bundled permissive components (whisper.cpp, libopus,
coder/websocket) keep their own licenses; their notices are retained.
