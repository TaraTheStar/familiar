# Release notes — Protocol v2 (DRAFT)

> **Status: draft for WS7.** Set the final version/tag and date before publishing.
> Suggested tag: `v2.0.0`. This is the first release on the Protocol v2 wire; it
> supersedes the v1 protocol entirely (v1 is removed server-side).

The `familiar` voice companion moves to **Protocol v2** end-to-end: a leaner,
first-class wire protocol between the StackChan firmware (`poppet`) and the
server (`grimoire`), proven on real hardware, plus three new features.

## ⚠️ Upgrade is not backward-compatible — a firmware reflash is REQUIRED

- The server is now **v2-only**. The v1 wire path and the legacy `/xiaozhi/`
  mounts are gone; the server serves `/grimoire/` only.
- A device on old firmware **cannot reach this server** until reflashed (its OTA
  probe 404s). Reflash to point `CONFIG_OTA_URL` at `…/grimoire/ota/`.
- See "Reflash checklist" below — this single reflash also lands the firmware-side
  halves of several features and is the **first hardware verification** of the
  firmware changes in this release.

## Highlights

- **Protocol v2 wire** — connection-is-the-session (no `session_id`), raw-Opus
  audio framing, credit-based audio flow control, first-class tools (no MCP
  envelope), first-class `error` frames, cumulative captions, clock-from-hello,
  read-idle heartbeat. Spec: `docs/PROTOCOL_V2.md`.
- **v2-only server** — v1 removed (migration phase 5).
- **Firmware vendored in-tree** — the firmware core is now a pruned in-repo fork
  (`poppet/vendor/stackchan-esp32/`); project Kconfig de-branded to `STACKCHAN_*`.
- **Three new features** (WS4): streaming ASR, an external-MCP tool adapter, and
  pluggable familiar faces.

## New in this release (WS4)

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

## Firmware (`poppet`)

- Protocol v2 port (raw-Opus framing, credit accounting, first-class tools,
  clock-from-hello, telemetry, goodbye/error).
- CPU cleanup: servo-bus blocking read, adaptive `face_det` idle cadence, emotion
  icon handling.
- Wake-word-during-speaking gated behind a Kconfig (off) — see deferral below.
- Vendored, de-branded firmware core.

## Deferred to v2.1 (post-release)

- **Barge-in / full-duplex (server-side AEC)** and its dependency **hello
  renegotiation**. **v1 ships half-duplex** — wait for the reply to finish before
  speaking again (the Stage-C-proven behavior). All the hooks are reserved in the
  v2 contract, so this is a clean fast-follow, not a re-architecture.
- **Per-word `caption.segments`** — current TTS (Kokoro) provides no word timing.

## Known limitations / not yet hardware-verified

- The **familiar faces** feature is build-verified but **not yet hardware-verified**
  (compositing, live swap, blink/talk animation, PNG-swap perf) — verified on the
  reflash. The shipped cat art is a placeholder.
- **Streaming ASR** device check (partial transcript updates vs. duplicates) is
  part of the reflash verification.
- The external MCP adapter requires the configured MCP server binaries to be
  available on the server host (e.g. `uvx mcp-server-fetch`).

## Reflash checklist (the one hard gate)

1. **Required:** menuconfig `CONFIG_OTA_URL` → `http://<server>:9099/grimoire/ota/`,
   then `idf.py build flash`. Without it the device can't find the server.
2. Optional firmware-send halves (latency/quality only): `wake_word_audio`
   pre-roll reorder + `tools_inline` population.
3. **Verify on device:** OTA probe 200s on `/grimoire/ota/` → WS connects on
   `/grimoire/` → happy-path turn; head-move during TTS has no audio glitch / no
   `task_wdt`; idle room shows low `face_det` CPU; emotions map cleanly.
4. **WS4 device checks:** "be a cat" swaps the avatar live (renders, blinks, mouth
   moves while speaking, persists across reboot; "be default" reverts); with
   `-asr-streaming` on, partial transcripts update the user line rather than
   duplicating.

## Full commit log

`git log main..` on `protocol-v2-server` — Stage A (reference client),
the v2 server, v1 removal, firmware v2 port + Stage-C bring-up fixes, firmware
vendoring + de-brand, and the WS4 features.
