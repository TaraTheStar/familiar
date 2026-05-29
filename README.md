# familiar

A self-hosted, local-network voice companion for the [StackChan](https://github.com/m5stack/StackChan)
ESP32-S3 robot. No cloud: the little robot on your desk wakes when called, hears
you, thinks, and speaks back — all on hardware you own.

The name is the witch's-companion kind: a *familiar* is a spirit bound to a
person. This repo holds the two halves of ours.

## Layout

```
familiar/
├─ poppet/      # the firmware — the little body that sits on your desk
│               #   (M5Stack StackChan app on a vendored, patched xiaozhi-esp32)
├─ grimoire/    # the backend — hears (whisper), thinks (LLM), speaks (Kokoro)
│               #   Go server, deployed as a container
└─ docs/        # the poppet↔grimoire contract
   ├─ PROTOCOL_V1.md       # the live v1 wire protocol grimoire implements
   └─ PROTOCOL_V2.md       # planned v2 redesign (draft, unimplemented)
```

- **[poppet/](poppet/)** — firmware. Build with ESP-IDF v5.5.4; run
  `python3 fetch_repos.py` first to pull and patch the vendored components.
  See [poppet/BUILD.md](poppet/BUILD.md).
- **[grimoire/](grimoire/)** — Go backend. `make build` (needs the whisper.cpp
  submodule). See [grimoire/README.md](grimoire/README.md).

## First clone

```
git clone <url> familiar
cd familiar
git submodule update --init --recursive   # pulls whisper.cpp for grimoire
```

## Origins

Both halves were extracted from earlier standalone repos (`StackChan` and
`go-stackend`) as a fresh-start snapshot; their pre-monorepo history lives in
those archives.

## License & acknowledgements

`familiar` is **dual-licensed by subtree**:

- **`grimoire/`** (the backend, original work) — **GNU AGPL v3.0-or-later**
  ([grimoire/LICENSE](grimoire/LICENSE)). Note §13: running a modified grimoire
  as a network service obliges you to offer users its source.
- **everything else** (`poppet/`, `docs/`, tooling) — **MIT** ([LICENSE](LICENSE)).

grimoire and poppet are separate programs talking over a socket, so the AGPL does
not extend across the wire to the MIT firmware.

It stands on a lot of other people's work — **poppet in particular is largely a
derivative**, not original code. With gratitude to:

**Firmware (poppet)**
- [m5stack/StackChan](https://github.com/m5stack/StackChan) — the StackChan
  robot and the base app `poppet/main/` is built from.
- [BrettKinny/StackChan](https://github.com/BrettKinny/StackChan) — the fork
  whose firmware work (`dotty` / firmware branches) poppet derives from.
- [78/xiaozhi-esp32](https://github.com/78/xiaozhi-esp32) @ v2.2.4 — the AI
  firmware base we vendor and patch (MIT, © 2025 Shenzhen Xinzhi Future
  Technology Co., Ltd. and Project Contributors).
- Bosch Sensortec [BMI270 SensorAPI](https://github.com/boschsensortec/BMI270_SensorAPI)
  under `poppet/main/hal/drivers/bmi270/` (BSD-3-Clause, © 2023 Bosch Sensortec GmbH).
- Components fetched by `poppet/fetch_repos.py`:
  [mooncake](https://github.com/Forairaaaaa/mooncake),
  [mooncake_log](https://github.com/Forairaaaaa/mooncake_log), and
  [smooth_ui_toolkit](https://github.com/Forairaaaaa/smooth_ui_toolkit) (Forairaaaaa);
  [ArduinoJson](https://github.com/bblanchon/ArduinoJson) (MIT);
  [esp-now](https://github.com/espressif/esp-now) (Espressif, Apache-2.0).

**Backend (grimoire)**
- [ggerganov/whisper.cpp](https://github.com/ggerganov/whisper.cpp) — on-device
  ASR (MIT, © 2023-2026 The ggml authors), vendored as a submodule.
- [Kokoro](https://github.com/hexgrad/kokoro) /
  [kokoro-fastapi](https://github.com/remsky/Kokoro-FastAPI) — TTS.
- An OpenAI-compatible LLM served via llama-swap (e.g. Google's Gemma, under the
  Gemma Terms of Use).

Each upstream remains under its own license; this repo retains their copyright
and license notices where their code is included.
