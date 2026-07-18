---
title: Sources
weight: 8
---

# Sources & lineage

familiar didn't spring from nothing — it's the newest branch of a family tree
of open projects, and this page is the map back to its ancestors. (For the
complete license-level credits of everything vendored or linked, see the
[repository README](https://github.com/TaraTheStar/familiar#license--acknowledgements).)

## The StackChan family — the body's ancestors

- **[stack-chan/stack-chan](https://github.com/stack-chan/stack-chan)** — the
  original. Shinya Ishikawa's "JavaScript-driven M5Stack-embedded
  super-kawaii robot" started the whole palm-sized-companion genre; every
  StackChan descends from this design and its open hardware.
- **[m5stack/StackChan](https://github.com/m5stack/StackChan)** — M5Stack's
  official open-source StackChan: the hardware poppet runs on and the base
  application `poppet/main/` is built from.
- **[BrettKinny/StackChan](https://github.com/BrettKinny/StackChan)** — the
  fork whose firmware work (the `dotty` / firmware branches) poppet directly
  derives from: the HAL, avatar, servo, and personality layers grew from
  here.

## The XiaoZhi family — the mind's ancestors

- **[78/xiaozhi-esp32](https://github.com/78/xiaozhi-esp32)** — the
  MCP-based ESP32 voice-assistant firmware. poppet's conversational core is
  a pruned in-tree fork of it (v2.2.4, at
  `poppet/vendor/stackchan-esp32/`) with all cloud endpoints removed.
- **[howecheung/StackChan-XiaoZhi](https://github.com/howecheung/StackChan-XiaoZhi)**
  — a kindred ancestor: StackChan body, xiaozhi mind, with a focus on
  *companionship* (touch, motion, mood lights, servos). Itself derived from
  [mo-hantang/Stackchan-HtSz](https://github.com/mo-hantang/Stackchan-HtSz).
  familiar walks the same StackChan-marries-xiaozhi path.
- **[xinnan-tech/xiaozhi-esp32-server](https://github.com/xinnan-tech/xiaozhi-esp32-server)**
  — the self-hosted backend for xiaozhi devices. grimoire replaces it for
  this project: same seat at the table, new wire protocol, LAN-only by
  design.

## The engines

The heavy lifting inside grimoire is other people's brilliant work:

- **[whisper.cpp](https://github.com/ggerganov/whisper.cpp)** — hearing
  (ASR), run in-process.
- **[Kokoro](https://github.com/hexgrad/kokoro)** via
  [Kokoro-FastAPI](https://github.com/remsky/Kokoro-FastAPI) — the voice
  (TTS).
- Any OpenAI-compatible LLM server — the thinking; we run ours through
  [llama-swap](https://github.com/mostlygeek/llama-swap).
- **[microWakeWord](https://github.com/OHF-Voice/micro-wake-word)** (Open
  Home Foundation) — the wake-word architecture behind "Hey Artemis", with
  [ESPMicroSpeechFeatures](https://github.com/kahrendt/ESPMicroSpeechFeatures)
  as the audio frontend.

## And this site

The docs' ink-and-parchment look riffs on the
[ensō](https://github.com/TaraTheStar/enso) documentation theme, steered a
few shades witchier.
