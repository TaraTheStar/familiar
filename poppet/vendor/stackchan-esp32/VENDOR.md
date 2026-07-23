# Vendored: stackchan-esp32 (firmware core)

This directory is a **pruned, vendored fork** of the upstream xiaozhi-esp32 ESP32
voice-assistant firmware, adopted into this repo as first-party source.

## Upstream

- **Project:** xiaozhi-esp32 — https://github.com/78/xiaozhi-esp32
- **Baseline:** tag **v2.2.4** (commit `e77dedb1309153bb63fed285772962c920c97dd4`)
- **License:** MIT © 2025 Shenzhen Xinzhi Future Technology Co., Ltd. and Project
  Contributors. The upstream `LICENSE` is retained verbatim in this directory.
  This `familiar` repo's `poppet/` subtree is also MIT, so the license is
  unchanged by vendoring (see the root `README.md` "License & acknowledgements").

## Why vendored instead of patched

Through 2026-05 this tree was fetched fresh at build time and a patch
(`poppet/patches/xiaozhi-esp32.patch`, ~1781 lines / 15 files) was applied on
top. That patch had grown from a config tweak into a full protocol-layer rewrite
(v1 → Protocol v2: `protocols/`, `mcp_server`, `application.cc`, `audio_service`,
`afe_wake_word`). Once the server went v2-only (PROTOCOL_V2 §10 Phase 4/5) we
committed to diverging from upstream, so the patch's only benefit — clean
`git rebase` onto new upstream — no longer applies. Vendoring puts the real
firmware source in this repo's history (blame, diffs, PR review) and removes the
fragile "edit-in-place then regenerate the patch" round-trip. The patch +
`fetch_repos.py` entry for xiaozhi-esp32 were deleted at adoption.

## What was pruned from upstream v2.2.4

Goal: keep only what the StackChan (m5stack-core-s3 / ESP32-S3) build compiles or
flashes. Removed:

- **`main/boards/` — all ~95 board variants except `common/`.** This project
  supports one board (StackChan); its board code lives in `poppet/main/`, not
  here. The build's `boards/${BOARD_TYPE}` glob (`BOARD_TYPE=m5stack-stack-chan`)
  has no dir here and resolves empty by design; only `boards/common/*.cc` is
  compiled.
- **`main/assets/locales/` — all 38 locales except `en-US`.** Build selects one
  via `CONFIG_LANGUAGE_*` (`en-US`); CMake globs only the selected `LANG_DIR`.
- **`build/`, the vendored-tree `.git/`, `docs/`, `.github/`, and the non-English
  `README_*.md`** — not consumed by the build.
- **The marketing `README.md` and upstream-only `scripts/` tooling.** The build
  invokes only `scripts/gen_lang.py` and `scripts/build_default_assets.py` (from
  CMake). Removed as unreferenced holdovers: `release.py`, `versions.py`,
  `download_github_runs.py` (xiaozhi CI/release automation); `ogg_converter/` and
  `p3_tools/` (this firmware plays OGG/Opus, not P3); `Image_Converter/` (a
  standalone LVGL image GUI); and `spiffs_assets/` (superseded — its
  `pack_model` / `build` / `spiffs_assets_gen` logic was inlined into
  `build_default_assets.py`). The upstream `README.md` was xiaozhi marketing
  whose every link pointed at already-pruned files. Kept dev tooling for live
  features: `audio_debug_server.py`, and the acoustic Wi-Fi provisioning
  (`sonic_wifi_config.html` + `acoustic_check/`, which drive the compiled-in
  AFSK demodulator).

Kept (needed by the build): `main/` sources, `main/boards/common/`,
`main/assets/{common,locales/en-US}` + `lang_config.h`, `scripts/` (asset/lang
codegen run from CMake via `${FIRMWARE_CORE_DIR}/../scripts`), `partitions/`,
the `sdkconfig.defaults*`, `CMakeLists.txt`, `LICENSE`. Result: ~16 MB → ~2 MB.

## How the build consumes this

`poppet/main/CMakeLists.txt` sets `FIRMWARE_CORE_DIR` to `../vendor/stackchan-esp32/main`
and pulls an explicit source list from it, plus `boards/common/*`, plus the
StackChan sources under `poppet/main/` itself. The external component libraries
(mooncake, mooncake_log, smooth_ui_toolkit, ArduinoJson, esp-now) are still
fetched by `poppet/fetch_repos.py` into `poppet/components/` (unmodified — no
reason to vendor them). ESP-IDF managed components stay under
`poppet/managed_components/` as usual.

## Updating from upstream (manual, no longer a rebase)

There is no automated upstream tracking. To pull a newer upstream you do a
manual 3-way merge: fetch the new tag into a scratch checkout, diff our `main/`
against it, and port what you want by hand — we have intentionally diverged
(v2-only protocol, no v1/MCP-JSON-RPC). Bump the baseline tag/commit above when
you do.

## Local changes vs upstream v2.2.4 (high level)

The full v1→v2 protocol rewrite that used to live in the patch (now applied
in-tree): `protocols/protocol.{cc,h}`, `protocols/websocket_protocol.{cc,h}`
(Protocol-Version: 2 header, `wake`/`listen_start`/`listen_stop`/`telemetry`
frames, no session_id, clock-from-hello, audio credits), `mcp_server.{cc,h}`
(first-class `tool_list`/`tool_call` instead of the JSON-RPC envelope),
`application.cc` (v2 dispatch, privacy gate), `audio_service.{cc,h}` (decode-
credit hook), `afe_wake_word.cc` (mutex fix), `Kconfig.projbuild`
(`CONFIG_WAKE_WORD_DETECTION_IN_SPEAKING`), `assets.{cc,h}`,
`boards/common/i2c_device.cc`. Going forward these are just normal commits in
this repo's history.
