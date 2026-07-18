# Wake word — microWakeWord "Hey Artemis"

How the prebuilt ESP-SR WakeNet9 wake word ("Hi, ESP") was replaced with a
custom-trained microWakeWord (TFLite-Micro) **"Hey Artemis"** model — the
decision record, the training recipe, and the flash/tuning steps. **Shipped
2026-07-18** (on-device: recall 6/6, adversarial phrases rejected, cutoff
0.70 / sliding window 4). The model is speaker-adapted: to use your own phrase
or voice, follow the same recipe with your own recordings.

## Why this path (decision record, 2026-07-15)

Three routes were evaluated:

1. **Prebuilt wakenet swap** (e.g. `wn9_hifairy_tts2` "Hi, Fairy"): zero
   effort, but limited to Espressif's phrase list; the `_tts*` models are
   synthetic-trained and a notch below the flagship models.
2. **MultiNet CustomWakeWord** (in-tree, zero training — the phrase as an
   English speech command in phonemes): **attempted and dead-ended on flash
   budget.** Enabling any English MultiNet hard-links `libflite_g2p.a`
   (+610 KB app code — text→phoneme data, not gateable without patching the
   esp-sr prebuilt) and needs `mn7_en` (2.7 MB) in assets. The app binary went
   5.00 → 5.62 MB and overflowed the 5.25 MB A/B OTA slots by 257 KB; fitting
   it required either dropping the A/B layout or squeezing to ~5 KB headroom.
   The config for that attempt (reverted, kept for reference):
   `USE_CUSTOM_WAKE_WORD=y`, `CUSTOM_WAKE_WORD="hd LoNc"` (g2p phonemes for
   "hey artemis" via `managed_components/espressif__esp-sr/tool/multinet_g2p.py`),
   `SR_MN_EN_MULTINET7_QUANT=y`, assets partition 4 M → 0x500000.
3. **microWakeWord (this plan)**: trained model is ~130 KB + a small
   TFLite-Micro runtime — fits the EXISTING partition layout with A/B OTA
   intact, and detection quality is the community-proven best of the three
   (it's ESPHome's production wake engine). Cost: training needs
   NVIDIA/Colab + real tuning effort.

Espressif's own custom-word training (`wn9_customword`) remains a
commercial, sales-mediated service (≥20k samples, 500+ speakers) — not an
option. Verified 2026-07: no self-serve esp-sr path exists.

## Implementation status (2026-07-15)

- **Firmware runtime: DONE + builds green.** `microwakeword.{h,cc}` written and
  wired (`SetModelsList` `#if CONFIG_USE_MICROWAKEWORD` branch + extended
  `IsAfeWakeWord()`). `USE_MICROWAKEWORD=y` build links clean and fits the OTA
  slot: binary 0x4e26b0, 5% (245 KB) free; A/B OTA preserved. Deps resolve
  (`esp-tflite-micro` ==1.3.7, `esp-micro-speech-features` git-pinned to
  351c4c6…a00, `esp-nn` transitive), gated via `$CONFIG{USE_MICROWAKEWORD}`.
- **Not yet verified on hardware** — needs a model in the `mww_model` partition.
  `hey_jarvis.tflite` (51 KB, Apache-2.0) is staged as the bench stand-in to
  prove the runtime independently of training; then `hey_artemis` drops in with no
  code change.
- **Two Kconfig knobs** carry the manifest's calibrated values:
  `MICROWAKEWORD_THRESHOLD` = `probability_cutoff`×100, `MICROWAKEWORD_SLIDING_WINDOW`
  = `sliding_window_size`. Stride, quantization, and arena adapt from the `.tflite`.

## Training (host side)

Upstream is **OHF-Voice/micro-wake-word** (Open Home Foundation; formerly
kahrendt/microWakeWord). Apache-2.0. Its `basic_training_notebook.ipynb` is
labeled advanced-users-only — don't run it raw; use the maintained wrapper.

**Training host = user's RTX 3090 (24 GB, Ampere / sm_86), decided
2026-07-15.** Well-supported by mainline TF-GPU; Colab and AMD/ROCm are moot.

**Trainer = [TaterTotterson/microWakeWord-Trainer-Nvidia-Docker]** (the de
facto standard as of mid-2026 — active weekly releases, web UI on :8789,
supersedes the dead ClarePhang/stujenn lineage). It wraps piper + the
kahrendt negatives + training + TFLite export behind one container.

```bash
# On the 3090 — STANDARD image (NOT the :vN-blackwell one; that's RTX 50-only)
docker run -d --gpus all --network host -e REC_PORT=8789 \
  -v $(pwd):/data ghcr.io/tatertotterson/microwakeword:latest   # pin :vN for repro
# then browse to http://<3090-host>:8789 → Trainer tab
```

- **Phrase**: enter "Hey Artemis" in the UI; preview TTS pronunciation first.
  If piper mangles it, use piper's `--phoneme-input` IPA path — the UI's
  plain-text default is usually fine for a soft phrase like this (no hard
  final stop to drop, unlike "Hey Frank" → `hˈeɪ fɹˈæŋk˺`).
- **Positives**: TTS-only works (zero personal recordings needed). Piper
  generates across voices + speaking-rates to reach the effective 10k–50k
  total. Can also upload a few device-captured "Hey Artemis" clips via the UI.
- **Negatives**: built-in stock sets (their HF mirror of RIR/impulse +
  backgrounds) ship in the image since v6; you add reviewed false-triggers
  in the UI. Still backed by kahrendt/microwakeword (**9.73 GB**, one-time
  download, cached). Feed in targeted hard negatives if a specific false-accept
  shows up on-device — e.g. "Hey Sam", or "Hey Arnaud" (a similar-sounding name).
  Keep the negative count modest (~single-digit multiple of the positive takes):
  flooding a small-positive model with a large narrow-negative set distorts the
  whole decision boundary (1920 negs broke it; ~200 balanced negs worked).
- **Thresholds**: v3+ **self-calibrates** `probability_cutoff` /
  `sliding_window_size` post-training against a validation set — no more
  hand-tuning. This removes the biggest historical pain point.
- **Output**: `/data/output/<ts>-hey_artemis-.../hey_artemis.{tflite,json}` (also
  synced to `/data/trained_wake_words/`). Streaming INT8 `.tflite` (~130 KB)
  + JSON manifest with `probability_cutoff`, `sliding_window_size`,
  `feature_step_size`, `tensor_arena_size`. NOTE: TaterTotterson's v10 JSON
  is a **superset** (extra Tater-specific keys) — our runtime parser must
  read only the four standard fields and ignore unknown keys.
- **Time**: ~1–3 h wall-clock first run (dominated by the 10 GB download +
  sample gen, not GPU compute — a 3090 has ample headroom); re-runs on the
  same phrase are much faster (datasets cached).

[TaterTotterson/microWakeWord-Trainer-Nvidia-Docker]: https://github.com/TaterTotterson/microWakeWord-Trainer-Nvidia-Docker

## Runtime (firmware side)

**License verdict (verified 2026-07-15): the clean path is fully open.**
Every library we link is Apache-2.0; the only GPL code is ESPHome's own
runtime, which we treat as algorithm reference and never copy.

- **Interpreter**: `espressif/esp-tflite-micro` — **Apache-2.0**, registry
  v1.3.7, declares `idf >=5.0` + `esp-nn >=1.1.1` (clean on IDF 5.5.4). Pin
  1.3.7 + a satisfying esp-nn (ESPHome's older 1.3.1/1.1.0 combo is stale).
- **Feature frontend — SEPARATE library, not a TFLM op.** ESPHome calls
  `FrontendProcessSamples()` (from TF's microfrontend) *before* the
  interpreter; the model only ever sees the 40 precomputed int8 features.
  So we MUST bring a frontend lib. Two Apache-2.0 choices:
  - `kahrendt/ESPMicroSpeechFeatures` (aka esp-micro-speech-features) —
    **Apache-2.0**, a fork of TF microfrontend by microWakeWord's own author,
    perf-tuned for Espressif. Not on the registry → add as a **git dep** in
    `idf_component.yml`, pinned to a commit. **Chosen** (purpose-built, small).
  - `tensorflow/.../lite/experimental/microfrontend` — Apache-2.0, canonical,
    but means vendoring a chunk of TF source. Fallback if the fork rots.
- **GPL — reference ONLY, never copy into this MIT tree**: ESPHome's
  `micro_wake_word.cpp/.h` (`StreamingModel`/`WakeWordModel`/`VADModel`) is
  **GPLv3** (their LICENSE: C/C++ files are GPLv3, Python is MIT);
  `0xD34D/micro_wake_word_standalone` is **GPL-3.0**. Read them for the
  algorithm only. `OHF-Voice/pymicro-wakeword` (**Apache-2.0**,
  `process_streaming`) is the legally-safe second-source algorithm reference.

- **Frontend feature params (must match training exactly)**: 16 kHz mono
  int16; 40 filterbank channels; 30 ms window; **10 ms step** (trust the
  manifest's `feature_step_size`, not the training util's 20 ms default);
  125–7500 Hz band; PCAN (strength 0.95, offset 80.0); log-scale (shift 6).

- **Streaming inference loop** (our own impl against the Apache libs; mirrors
  `pymicro-wakeword.process_streaming`):
  1. Ring-buffer incoming 16 kHz int16 PCM (AFE-cleaned `res->data`).
  2. Every `feature_step_size` ms (10 ms = 160 samples), run the frontend
     over the 30 ms window → one 40-value feature vector.
  3. Quantize to the model's input tensor — **read scale/zero-point from the
     `.tflite` at runtime** (interpreter API); do NOT hardcode ESPHome's
     `(f*256)/666-128` constant (that's tied to their feature scale).
  4. Copy 40 int8 features into the input tensor, `Invoke()`. The v2
     streaming model keeps 3-slice temporal history internally — we feed ONE
     frame per invoke, not an accumulated window.
  5. Dequantize the single output probability (output tensor scale/zp).
  6. Push into a ring of length `sliding_window_size`; if the windowed
     average > `probability_cutoff` → fire, then apply our own cooldown
     (ignore-window) before re-arming.

- **Tensor arena**: the manifest's `tensor_arena_size` is the interpreter
  arena ONLY (hey_jarvis: ~22.8 KB). Frontend `FrontendState` is a separate
  few-KB alloc. A single model + frontend + 120 ms ring (~3.8 KB) + task
  stack (3072 B) fits ESP32-S3 internal SRAM without PSRAM. (We can still put
  the arena in PSRAM via the AFE's MORE_PSRAM convention to save internal RAM.)

- **Model-agnostic by design → testable before Hey Artemis exists.** Because
  quant params come from the `.tflite` and the 4 detection params come from a
  flashed/generated manifest, the firmware runs ANY microWakeWord v2 model.
  Build + functionally smoke-test it against the **public Apache-2.0
  `hey_jarvis.tflite` + manifest** from `esphome/micro-wake-word-models`
  first — it will actually detect "Hey Jarvis" on the bench — then swap in
  `hey_artemis` with no code change. This de-risks all of Track B independently
  of the 3090 training run.

- **Manifest**: parse only the 4 standard fields
  (`probability_cutoff`, `sliding_window_size`, `feature_step_size`,
  `tensor_arena_size`); TaterTotterson's trainer emits a superset — ignore
  unknown keys. Bake into a generated header at build time OR flash the JSON
  into a small partition and parse at boot.

Add to `poppet/main/idf_component.yml` (our component, NOT the vendored one):

```yaml
dependencies:
  espressif/esp-tflite-micro: "==1.3.7"
  esp-micro-speech-features:
    git: https://github.com/kahrendt/ESPMicroSpeechFeatures.git
    # pin to a specific commit hash once vendored, for reproducibility
```

### Partition

Append to `poppet/partitions.csv` (256 KB comfortably holds the ~130 KB
model + manifest; 1.3 MB is unallocated at the end of the 16 MB flash today):

```
mww_model, data, fat,    ,         0x40000,
```

Flash the model with
`parttool.py --port <PORT> write_partition --partition-name=mww_model --input model.tflite`
(or fold it into the assets pipeline later — assets has ~1.7 MB free).

### Source layout

New files under `poppet/main/stackchan/wake_word/`:

```
microwakeword.h         // class MicroWakeWord : public WakeWord (mirror AfeWakeWord)
microwakeword.cc        // streaming inference task, tensor arena, threshold logic
mww_model_loader.cc     // mmap the mww_model partition into PSRAM
```

Mirror the `WakeWord` interface from
`poppet/vendor/stackchan-esp32/main/audio/wake_word.h` (10 pure-virtual
methods) so `AudioService::SetModelsList` can hold it behind the same
`std::unique_ptr<WakeWord>`. Keep AFE (AEC/NS) in FRONT of the model:

```
Mic ──▶ AFE (AEC, NS, VAD) ──▶ MicroWakeWord (TFLite streaming) ──▶ wake event
```

Feed AFE-cleaned PCM, not raw mic. Fork `AfeWakeWord` and swap the detector:
the whole pre-roll capture/encode/opus path (`StoreWakeWordData`,
`EncodeWakeWordData`, `GetWakeWordOpus`) is copied verbatim — only
`AudioDetectionTask`'s `res->wakeup_state == WAKENET_DETECTED` check is
replaced with the TFLite streaming loop over `res->data`.

**Confirmed wiring against the installed tree (esp-sr 2.3.0, verified
2026-07-15):**

- **AFE runs wakenet-free.** `esp_afe_config.h` (esp32s3, line 121) exposes
  `bool wakenet_init;` — set `afe_config->wakenet_init = false` in
  `Initialize()` so AFE does AEC/NS/VAD only and never loads a WakeNet. The
  rest of the config mirrors `afe_wake_word.cc:73-81` (AFE_TYPE_SR,
  AFE_MODE_HIGH_PERF, core 1, MORE_PSRAM). No wakenet model needs to ship in
  `srmodels.bin` for the MWW build (frees assets space).
- **Selection is compile-time, not data-driven.** The MWW `.tflite` lives in
  the `mww_model` partition, not `srmodels.bin`, so `SetModelsList` can't pick
  it via `esp_srmodel_filter`. Add a branch at the TOP of
  `AudioService::SetModelsList` (`audio_service.cc:749`):
  ```cpp
  #if CONFIG_USE_MICROWAKEWORD
      wake_word_ = std::make_unique<MicroWakeWord>();
  #elif CONFIG_IDF_TARGET_ESP32S3 || CONFIG_IDF_TARGET_ESP32P4
      ... existing MN/WN selection ...
  ```
- **`IsAfeWakeWord()` must return true for MWW.** It gates whether on-device
  wake detection is re-enabled after each turn
  (`application.cc:1151,1176` → `EnableWakeWordDetection(IsAfeWakeWord())`).
  MicroWakeWord is an always-listening on-device detector, so extend the
  predicate (`audio_service.cc:777`) with
  `|| dynamic_cast<MicroWakeWord*>(wake_word_.get())` (or rename to
  `IsOnDeviceWakeWord()`). Miss this and wake detection silently stops
  after the first conversation.

### Kconfig

In `poppet/main/Kconfig.projbuild` (ours, not the vendored one):

```
config USE_MICROWAKEWORD
    bool "microWakeWord (custom Hey Artemis TFLite)"
    depends on (IDF_TARGET_ESP32S3 || IDF_TARGET_ESP32P4) && SPIRAM
    select USE_AFE_WAKE_WORD

config MICROWAKEWORD_THRESHOLD
    int "detection threshold (0-99)"
    default 50
    range 1 99
    depends on USE_MICROWAKEWORD
```

The wake-word string sent via `protocol_->SendWakeWordDetected()` becomes
`"Hey Artemis"`; the server treats it as informational (verified — arbitrary
phrases already flow through).

## Test plan

1. Flash model to `mww_model`; boot log prints model size on successful mmap.
2. "Hey Artemis" 10× from 1.5 m, quiet room → expect 10/10 at the manifest's
   threshold.
3. 1-hour silence + TV-on soak → ≤1 false accept.
4. Hard negatives: "Hey Sam", "Hey Arnaud" (French ar-NOH), similar names → 0.
5. CPU/memory: baseline vs +MWW; budget ~5–8% core-1 CPU, ~250–300 KB PSRAM
   (tensor arena — benchmark SPIRAM vs internal for first-inference latency).
6. A week of household soak; review server wake logs (`msg=wake`).

## Rollback

`USE_MICROWAKEWORD=n` → prebuilt WakeNet9 path (`SR_WN_WN9_*`) untouched.
The `mww_model` partition can stay flashed but unread.
