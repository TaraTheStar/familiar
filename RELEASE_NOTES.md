# Release notes — v2.0.0

> **Tagged 2026-07-18.** Two tags mark this repo's history:
>
> - **`v1.0.0`** — the initial snapshot, at parity with the previous
>   standalone projects: it speaks the v1 wire protocol and remains
>   backward-compatible with the upstream xiaozhi-esp32 v2.2.4 firmware base.
>   Check it out if you need the old wire.
> - **`v2.0.0`** — this release: the first on the Protocol v2 wire, superseding
>   v1 entirely (v1 is removed server-side; upgrading a v1 device requires a
>   reflash, see below).

The `familiar` voice companion moves to **Protocol v2** end-to-end: a leaner,
first-class wire protocol between the StackChan firmware (`poppet`) and the
server (`grimoire`), proven on real hardware, plus three new features.

## ⚠️ Upgrade is not backward-compatible — a firmware reflash is REQUIRED

- The server is now **v2-only**. The v1 wire path and the legacy `/xiaozhi/`
  mounts are gone; the server serves `/grimoire/` only.
- A device on old firmware **cannot reach this server** until reflashed (its OTA
  probe 404s). Reflash to point `CONFIG_OTA_URL` at `http://<server>:9099/discover`
  (recommended; the legacy `…/grimoire/ota/` rich shape also works).
- See "Reflash checklist" below — the single reflash also lands the firmware-side
  halves of several features (wake pre-roll, familiar faces, the custom wake word).

## Highlights

- **Protocol v2 wire** — connection-is-the-session (no `session_id`), raw-Opus
  audio framing, credit-based audio flow control, first-class tools (no MCP
  envelope), first-class `error` frames, cumulative captions, clock-from-hello,
  read-idle heartbeat. Spec: `docs/PROTOCOL_V2.md`.
- **v2-only server** — v1 removed (migration phase 5).
- **Firmware vendored in-tree** — the firmware core is now a pruned in-repo fork
  (`poppet/vendor/stackchan-esp32/`); project Kconfig de-branded to `STACKCHAN_*`.
- **Three new features**: streaming ASR, an external-MCP tool adapter, and
  pluggable familiar faces.
- **Custom wake word — "Hey Artemis"** — a microWakeWord TFLite streaming model
  replaces the stock WakeNet phrase, trained on personal voice clips + balanced
  hard-negatives. Training/tuning recipe: `docs/WAKE_WORD.md`.
- **User docs** — `docs/` is now a full user guide (and a Hugo-built GitHub
  Pages site): quickstart, day-to-day usage, deep dives on both halves, and
  familiar screenshots rendered from the shipped sprite assets.

## New in this release

- **Streaming ASR (opt-in).** With `-asr-streaming`, the server emits incremental
  `transcript{final:false}` partials while you speak, then the authoritative
  `final:true`. Off by default (each partial is a whisper re-inference).
- **External MCP tool adapter.** `-mcp-config <file>` bridges external standard-MCP
  servers into the LLM's tool catalog (namespaced `mcp__<server>__<tool>`,
  dispatched server-side). Primary use case: a **`fetch`** server for internet
  reach (weather, news). See `grimoire/mcp.example.json`. *(LLM-facing only; the
  server still does not answer device-initiated tool calls.)*
- **Pluggable familiar faces.** Selectable avatar "familiars" (e.g. a **cat**) on
  top of the existing emotion/animation system, switchable by voice via the
  `self.avatar.set_familiar` tool and persisted in NVS. Ships a placeholder cat;
  add more by dropping `<name>_*.png` art and registering the name.

## Server (`grimoire`)

- Protocol v2 implementation, reference v2 client (conformance oracle), and v2
  wire types/decoder/fuzzer.
- Dictated audio (16 kHz mic / 16 kHz TTS) with server-side 24→16 kHz resampling;
  `-tts-tail-pad-ms` to avoid clipping the last word.
- `wake_word_audio` pre-roll and `tools_inline` honored; `/discover` endpoint;
  `battery_low` telemetry → device alert.
- `/grimoire/` paths (the `/xiaozhi/` rebrand).
- Structured JSON logs, a wake-confidence gate, and a whisper log bridge.
- Hardened by a pre-release review pass (2026-07): read-loop/turn-race fixes,
  dialogue-history cap, bounded credit loops, TTS tail-clip root-cause fix
  (in-flight playback accounting), 16 kHz defaults throughout.

## Firmware (`poppet`)

- Protocol v2 port (raw-Opus framing, credit accounting, first-class tools,
  clock-from-hello, telemetry, goodbye/error).
- CPU cleanup: servo-bus blocking read, adaptive `face_det` idle cadence, emotion
  icon handling.
- Wake-word-during-speaking gated behind a Kconfig (off) — see deferral below.
- Vendored, de-branded firmware core.
- **microWakeWord "Hey Artemis"** wake word (custom TFLite model in the
  `mww_model` partition, run behind the AFE with WakeNet disabled; cutoff 0.70,
  sliding window 4). Note: the shipped model is adapted to its author's voice —
  other voices should retrain per `docs/WAKE_WORD.md`.
- `/discover` lean boot path; `display.status` + `alert.sound` token mapping;
  `battery_low` telemetry emission; standalone `error{PROTOCOL_VIOLATION}` on
  malformed frames.

## Deferred to v2.1 (post-release)

- **Barge-in / full-duplex (server-side AEC)** and its dependency **hello
  renegotiation**. **v1 ships half-duplex** — wait for the reply to finish before
  speaking again. All the hooks are reserved in the
  v2 contract, so this is a clean fast-follow, not a re-architecture.
- **Per-word `caption.segments`** — current TTS (Kokoro) provides no word timing.

## Known limitations

- The external MCP adapter requires the configured MCP server binaries to be
  available on the server host (e.g. `uvx mcp-server-fetch`).
- The wake-word model is speaker-adapted (trained on its author's voice); other
  voices should retrain their own model per `docs/WAKE_WORD.md`.
- Barge-in is deferred — speaking over the reply does nothing until it finishes
  (half-duplex; see "Deferred to v2.1").

Everything in this release was **verified live on hardware (2026-07-15/18)**:
happy-path turns, wake pre-roll from idle, familiar-face live swap + animation,
streaming-ASR display updates, tools, telemetry, `/discover` cold boot, and the
custom wake word (recall 6/6, adversarial phrases rejected).

## Reflash checklist (upgrading a device from v1 firmware)

1. **Required:** menuconfig `CONFIG_OTA_URL` → `http://<server>:9099/discover`,
   then `idf.py build flash`. Without it the device can't find the server.
2. Flash the wake-word model to the `mww_model` partition (see
   `docs/WAKE_WORD.md`), or disable `CONFIG_USE_MICROWAKEWORD` to fall back to
   WakeNet's stock phrase.
3. **Verify on device:** OTA probe 200s → WS connects on `/grimoire/` →
   happy-path turn; "be a cat" swaps the avatar live; with `-asr-streaming` on,
   partial transcripts update the user line rather than duplicating.

## Full commit log

`git log v1.0.0..v2.0.0` — the reference conformance client, the v2 server,
v1 removal, firmware v2 port + hardware bring-up fixes, firmware vendoring +
de-brand, the three new features, the pre-release review fixes, the
"Hey Artemis" wake word, and the user docs.
