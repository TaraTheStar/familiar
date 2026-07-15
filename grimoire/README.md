# grimoire

The backend half of [familiar](../README.md) — the local-first voice-loop server
for the StackChan ESP32 robot ([poppet](../poppet/)).

Replaces [xiaozhi-esp32-server](https://github.com/xinnan-tech/xiaozhi-esp32-server) for
self-hosted deployments. Implements the v2 wire protocol that the
[poppet firmware](../poppet/) speaks — v2-only since the Phase-4 cutover.

## Status

Live. The full voice loop (mic → ASR → LLM → TTS → speaker) is verified on hardware.

## What it does

Mediates between a StackChan robot and a stack of locally-hosted AI services:

```
device ──WS──> stackend ──HTTP──> llama-swap   (LLM)
                       ├─────────> whisper.cpp  (ASR, in-process via cgo)
                       └─────────> Kokoro       (TTS)
```

See [../docs/PROTOCOL_V2.md](../docs/PROTOCOL_V2.md) for the live wire protocol
this implements ([../docs/PROTOCOL_V1.md](../docs/PROTOCOL_V1.md) is the retired
predecessor, kept for history).

## Layout

```
cmd/stackend/        # entry point (binary: stackend)
cmd/v2client/        # reference v2 client (protocol smoke-testing)
internal/
  protov2/           # v2 wire format — JSON message types + decode
  protocol/          # shared AudioParams + OTA HTTP response types
  session/           # per-connection state machine; the voice loop
  audio/             # Opus encode/decode, frame muxing
  asr/               # whisper.cpp ASR (cgo, model bundled in image)
  llm/               # OpenAI-compatible client (talks to llama-swap)
  tts/               # Kokoro client + streaming sentence chunking
  mcptools/          # external MCP adapter — bridges standard MCP servers'
                     #   tools to the LLM (server-side; never on the device wire)
  vision/            # /vision endpoint — camera captures to a multimodal LLM
  v2client/          # client-side v2 protocol library (backs cmd/v2client)
  ota/               # /ota/, /grimoire/ota/, /discover HTTP endpoints
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

## External MCP tools (optional)

`-mcp-config=<file>` bridges standard MCP servers into the LLM's tool catalog
(namespaced `mcp__<server>__<tool>`, dispatched server-side — the device never
sees them). Copy `mcp.example.json`, adjust the server commands, and pass the
flag; the process must be able to launch what the config names (e.g. `uvx`).
The committed `compose.yaml` ships with the flag commented out because the
default container image carries no MCP launchers — mount your config and a
suitable image layer to enable it.

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
