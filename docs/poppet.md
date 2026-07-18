---
title: "Deep dive: poppet"
weight: 4
toc: true
---

# Poppet — the body

A deep dive into the firmware half of familiar: the ESP32-S3 program that runs
the M5Stack StackChan — its ears, voice, face, neck, and the on-device wake
word. Built with ESP-IDF v5.5.4; build instructions live in
[`poppet/BUILD.md`](https://github.com/TaraTheStar/familiar/blob/main/poppet/BUILD.md).

## Two layers: the core and the character

Poppet is deliberately two code bases in one tree:

- **`poppet/vendor/stackchan-esp32/`** — the voice-assistant core, a pruned
  in-tree fork of [xiaozhi-esp32](https://github.com/78/xiaozhi-esp32) v2.2.4.
  It owns the hard plumbing: audio codecs and the acoustic front-end
  (AEC/noise-suppression/VAD), the Protocol v2 WebSocket client, the
  conversation state machine, OTA/discovery, NVS settings. Every cloud
  endpoint upstream shipped with has been removed — the fork is LAN-only by
  construction, and it's committed in-tree (no patch stack; edit it directly).
- **`poppet/main/`** — the character. The HAL for the StackChan's actual
  hardware (servos, IMU, RTC, head-touch sensor, LED rings, camera), the
  animated avatar with its familiar skins, idle motion, the setup UI, and the
  microWakeWord engine.

The only seam between them is `main/hal/board/hal_bridge.{h,cc}` — a small
namespace the character layer uses to reach the core (start the assistant,
lock the display, play a sound, toggle listening) without the two worlds
including each other's headers.

## Boot to conversation

1. **Power on** → HAL init, boot logo. If the device is unconfigured (or you
   asked for settings), a touch UI handles Wi-Fi provisioning (hotspot by
   default) and servo calibration.
2. **Discovery.** The device GETs the URL you flashed into `CONFIG_OTA_URL` —
   your server's `/discover` endpoint — and receives `{ws_url: …}`. That is
   the *only* address baked into the device; the server can move as long as
   discovery answers. If the URL is blank, the device refuses to open any
   socket at all and idles — fail-safe, zero egress.
3. **Handshake.** It opens the WebSocket with `Protocol-Version: 2`, sends its
   `hello` (codec, features, its tool catalog inline), and gets back the
   server's `hello` — which dictates audio parameters *and* carries the wall
   clock, so an unconfigured-NTP device still knows the time.
4. **Idle.** The face appears, idle motion starts, and the wake-word engine
   listens. From here the loop is: wake → stream mic audio → server replies
   with captions + Opus speech → back to idle.

The device and the wire are **half-duplex** in this release: while the
familiar speaks, the mic is off. Audio is Opus, 16 kHz mono, 60 ms frames both
directions, with credit-based flow control so the ESP32's fixed decode buffer
can never overrun ([Protocol v2](PROTOCOL_V2.md) has the full contract).

## The wake word

The stock firmware wakes on Espressif's prebuilt WakeNet phrases ("Hi, ESP" et
al.). Poppet instead ships a **microWakeWord** engine
(`main/stackchan/wake_word/microwakeword.{h,cc}`) — the streaming
TFLite-Micro architecture ESPHome uses in production — running a custom
**"Hey Artemis"** model:

- The ESP-SR acoustic front-end still runs (noise suppression, VAD) but with
  WakeNet off; its cleaned audio feeds a mel frontend and an int8 streaming
  model (~130 KB).
- The model lives in its own `mww_model` flash partition, so retraining is a
  partition flash, not a firmware rebuild — and the dual-slot OTA layout
  survives untouched.
- Detection threshold and sliding window are Kconfig options
  (shipped 0.70 / 4).
- On wake, the pre-roll buffer is forwarded as the start of the turn
  (`SEND_WAKE_WORD_DATA`), so the server hears what you said *while* it was
  waking up — and can optionally double-check the wake itself
  (grimoire's [wake gate](grimoire.md#observability)).

The shipped model is trained on its author's voice. [Wake word](WAKE_WORD.md)
is the full recipe for training your own phrase — including the hard-won
lesson that hard-negative counts must stay modest — and `menuconfig` can
always fall back to the stock WakeNet phrases.

## The face: avatars and familiars

`main/stackchan/avatar/` is a small rig, not a video player:

- An `Avatar` is a set of **features** (two eyes, a mouth, a speech bubble)
  positioned on a 320×240 panel.
- **Modifiers** animate features by writing integer *weights* each tick:
  `BlinkModifier` drops the eye weight every ~5 seconds, `SpeakingModifier`
  oscillates the mouth (and nods the head) while TTS plays, plus breath, idle
  gaze, thinking, IMU-reactive and head-pet modifiers. Emotions from the
  server map to weight presets — sleepy eyes droop, surprise widens.
- **Skins** decide how a weight becomes pixels. The `default` skin draws the
  classic StackChan face procedurally. The **familiar skins** (cat, bat, toad,
  fox) draw a full-screen costume PNG with sprite eyes and mouth on top —
  same rig, same modifiers, so a toad blinks exactly when the default face
  would have.

The switch is a device-side tool: the LLM calls `self.avatar.set_familiar`,
the registry (`avatar/skins/familiar/familiar_registry.cpp`) builds the new
avatar under the LVGL lock and swaps it live, and the choice persists in NVS.
The valid-name list in the tool description is generated from the registry, so
the LLM's menu can never drift from the code.

**Adding your own familiar** is three steps: draw (or script — see
`main/assets/familiars/gen_familiars.py`) five sprites
(`<name>_face.png` 320×240, `<name>_eye_open/closed.png`,
`<name>_mouth_open/closed.png`), drop them into `main/assets/assets_bin/`,
and add the name to `knownFamiliars()`. Rebuild, reflash the assets image, and
"be a dragon" works.

## The neck: servos and idle motion

Two Feetech SCS serial servos (yaw + pitch) give the head its life, wrapped in
`main/stackchan/motion/` with critically-damped spring physics — moves ease in
and settle rather than snap, and torque releases after settling so the servos
aren't buzzing at hold all day.

Idle motion runs on profiles (normal, looking-around, sleepy, surveillance)
with randomized cadence, and it's polite about it: face-tracking mode halves
the amplitude so the head doesn't snap away from a person it's looking at, an
empty room backs the cadence off to save servo lifespan, and sleep parks the
head and cuts torque so it *droops* — asleep, not powered-down.

## The rest of the body

- **Hardware map** (`main/hal/`): BMI270 IMU, PCF8563 RTC, capacitive
  head-touch sensor, an IO expander, two LED rings (one for state, one the LLM
  may color via `set_led_color`), and an optional camera whose stills feed the
  server's vision endpoint.
- **Device tools** (`main/hal/hal_mcp.cpp`): the catalog the LLM sees — head
  angles, LEDs, state (sleep), toggles (kid mode), reminders, and the familiar
  switch. Adding a capability here is how you teach the familiar a new trick.
- **Captions** render through the vendored display interface: the server's
  caption frames land in the avatar's speech bubble; status changes drive the
  listening pip and the speaking animation.

## Build system notes

- `fetch_repos.py` clones five unmodified UI/component libraries into
  `components/` (gitignored); everything modified is committed in-tree.
- Partitions (`partitions.csv`): dual 5.1 MB OTA app slots, a 4 MB assets
  partition (fonts, emoji, familiar sprites, sound effects), and two small FAT
  partitions for the face-detection and wake-word models.
- The Kconfig menu (`main/Kconfig.projbuild`) holds the deployment-specific
  bits — OTA/discovery URL and NTP hosts ship **intentionally blank**; there
  is no cloud default to fall back to.
- Firmware version comes from `git describe`, and lands in both the boot logo
  and the discovery exchange.

For toolchain setup, flashing, and the network-isolation guide (the actual
privacy boundary), see
[`poppet/BUILD.md`](https://github.com/TaraTheStar/familiar/blob/main/poppet/BUILD.md).
