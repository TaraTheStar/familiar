---
title: Quickstart
weight: 2
---

# Quickstart — from clean clone to a talking familiar

The shortest path to a StackChan on your desk holding a conversation with a
server you own. Everything runs on your LAN; nothing touches a cloud.

Deeper docs when you need them: [poppet/BUILD.md](https://github.com/TaraTheStar/familiar/blob/main/poppet/BUILD.md) (firmware
toolchain + network isolation), [grimoire/README.md](https://github.com/TaraTheStar/familiar/blob/main/grimoire/README.md)
(server internals + flags), [PROTOCOL_V2.md](PROTOCOL_V2.md) (the wire contract),
[WAKE_WORD.md](WAKE_WORD.md) (training a custom wake word for your own voice).

## What you need

- **The robot** — an M5Stack StackChan (ESP32-S3) with mic + speaker.
- **A server box** — any Linux machine on the same LAN, with `podman` (or
  docker) + compose. CPU-only is fine: whisper `base.en` ASR and Kokoro TTS
  both run comfortably on a modest box.
- **An LLM endpoint** — any OpenAI-compatible chat-completions server on your
  LAN (llama.cpp / llama-swap / vLLM / …) and, if it wants one, its API key.
- **A build machine** — a Linux box for compiling both halves (can be the
  server box). The firmware build needs ESP-IDF (§3).

Two addresses matter below; substitute yours throughout:

| placeholder     | meaning                                | example      |
| --------------- | -------------------------------------- | ------------ |
| `<LAN_IP>`      | the server box running grimoire        | `192.0.2.10` |
| `<LLM_HOST>`    | the box serving the LLM API            | `192.0.2.20` |

## 1. Clone

```bash
git clone <repo-url> familiar && cd familiar
git submodule update --init --recursive   # whisper.cpp (pinned, vendored)
```

## 2. Server (grimoire)

```bash
cd grimoire

# One time: download the ASR model (~142 MB). It gets bundled into the image.
make base-model

# Per-deployment config. LAN_IP, LLM_URL, LLM_MODEL, LLM_API_KEY, TIMEZONE.
cp .env.example .env
$EDITOR .env

# Optional: give your familiar its voice/personality.
$EDITOR persona.txt

# Build + start stackend and the Kokoro TTS sidecar.
podman compose up -d --build
```

The first build is the slow one (it compiles whisper.cpp and the Go binary
inside the image); rebuilds cache those layers.

**Verify it's alive:**

```bash
curl http://<LAN_IP>:9099/discover
# → {"ws_url":"ws://<LAN_IP>:9099/grimoire/"}

podman compose logs -f stackend   # watch for the device connecting later
```

That `/discover` URL is exactly what you'll flash into the firmware in §3 —
the device fetches it on boot to learn where the WebSocket lives.

> **Building on a different box than the deploy host?** Build the image where
> you have the checkout, then ship it:
> `podman save localhost/stackend:dev | ssh <server> podman load`, copy
> `compose.yaml`, `.env`, and `persona.txt` to the server box, and run
> `podman compose up -d` there (drop `--build`).

**Prove a full turn before touching the robot (optional but recommended):**
the repo ships a reference v2 client that speaks the same wire protocol as the
firmware. Point it at your server with any 16 kHz mono WAV as the "spoken"
turn — the whisper.cpp submodule includes one:

```bash
cd grimoire
GOWORK=off go build -o /tmp/v2client ./cmd/v2client   # needs libopus-dev
/tmp/v2client -url ws://<LAN_IP>:9099/grimoire/ \
    -mic third_party/whisper.cpp/samples/jfk.wav -out /tmp/reply.wav
```

A healthy run prints the handshake, the transcript of the WAV, the reply
caption, and writes the familiar's spoken answer to `/tmp/reply.wav` — that's
ASR → LLM → TTS all exercised end-to-end. If this works, any remaining trouble
is on the device side, not the server.

## 3. Firmware (poppet)

Full toolchain details live in [poppet/BUILD.md](https://github.com/TaraTheStar/familiar/blob/main/poppet/BUILD.md); the
short version:

```bash
# One time: install ESP-IDF v5.5.4 (idf.py ships inside the SDK).
git clone -b v5.5.4 --recursive https://github.com/espressif/esp-idf.git ~/esp/esp-idf
~/esp/esp-idf/install.sh esp32s3

cd poppet
. ~/esp/esp-idf/export.sh     # every new shell you build from
python3 ./fetch_repos.py      # one time: pull component libraries

idf.py set-target esp32s3
idf.py menuconfig
```

In `menuconfig`, under **Xiaozhi Assistant**, set the two intentionally-blank
options:

- **Default OTA URL** → `http://<LAN_IP>:9099/discover`
- **NTP server 1** → your LAN NTP host (see BUILD.md for when it's optional)

Then plug the StackChan in over USB and:

```bash
idf.py build
idf.py -p /dev/ttyACM0 flash monitor    # port is often /dev/ttyUSB0; Ctrl-] exits
```

## 4. First boot — what you should see

In `idf.py monitor`, the boot sequence should read:

1. Wi-Fi associates (first boot opens a provisioning AP if none configured).
2. The OTA check hits `/discover` and gets the `ws_url` back.
3. The WebSocket connects and the v2 `hello` handshake completes.
4. The face appears and the device settles into idle.

On the server side, `podman compose logs -f stackend` shows the session
starting at the same moment.

## 5. Say hello

- **"Hey Artemis"** (the shipped wake word) — the face perks up to listening.
  Caveat: the bundled microWakeWord model is trained on its author's voice and
  needs its model flashed to the `mww_model` partition
  ([WAKE_WORD.md](WAKE_WORD.md) covers both flashing it and retraining for
  your own voice/phrase). To skip all that, disable
  `CONFIG_USE_MICROWAKEWORD` in `menuconfig` and use a stock WakeNet phrase
  instead — esp-sr ships ~30 selectable English options ("Hi, ESP",
  "Hi, Fairy", "Hey, Willow", "Astrolabe", "Jarvis", …) under
  **ESP Speech Recognition → wakenet model**; no code changes. The
  human-recorded models (Hi ESP, Alexa) detect a notch better than the
  synthetic-trained ones (`_tts` suffix) — test your pick from across the
  room before settling.
- Ask something. Your words appear on the display as you speak, then the
  reply streams back with captions while the familiar talks.
- **"What time is it?"** — exercises a server-side tool.
- **"Be a cat."** — swaps the avatar to the cat familiar (and persists across
  reboots; "be default" switches back). Four sprite familiars ship: cat, bat,
  toad, and fox.
- **"Go to sleep."** — the familiar finishes its goodbye, then sleeps. Wake it
  with the wake word.
- **"Shut down."** — ends the conversation session (the device stays powered
  and reconnects idle). Actually powering off or rebooting is physical — there
  is deliberately no voice path to it.

## 6. Where to go next

- **Day-to-day usage** — everything it can do once it's talking:
  [using.md](using.md).
- **How each half works** — deep dives on the firmware ([poppet.md](poppet.md))
  and the server ([grimoire.md](grimoire.md)).
- **Network isolation** — the actual privacy guarantee is a firewall rule,
  not the firmware: [poppet/BUILD.md §5](https://github.com/TaraTheStar/familiar/blob/main/poppet/BUILD.md).
- **Give the LLM tools** — point `-mcp-config` at external MCP servers:
  [grimoire/README.md](https://github.com/TaraTheStar/familiar/blob/main/grimoire/README.md).
- **How the wire works** — [PROTOCOL_V2.md](PROTOCOL_V2.md).
