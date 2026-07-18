# poppet

The firmware half of [familiar](../README.md) — an M5Stack StackChan ESP32-S3
robot app that wakes on its wake word, streams your voice to
[grimoire](../grimoire/) over the Protocol v2 WebSocket wire
([docs/PROTOCOL_V2.md](../docs/PROTOCOL_V2.md)), and plays back the reply while
the avatar emotes. Built on an in-tree vendored fork of xiaozhi-esp32 at
[`vendor/stackchan-esp32/`](vendor/stackchan-esp32/); all cloud code is
removed — it talks only to your LAN server.

## Build

**[BUILD.md](BUILD.md) is the canonical build doc** — SDK install, the
mandatory `menuconfig` settings (OTA URL, NTP), flashing, and network
scoping live there. The short version:

```bash
. ~/esp/esp-idf/export.sh    # ESP-IDF v5.5.4 (idf.py ships inside the SDK)
python3 ./fetch_repos.py     # one-time: pull component libraries (needs network)
idf.py set-target esp32s3
idf.py menuconfig            # REQUIRED: set Default OTA URL (no cloud defaults)
idf.py build
idf.py -p /dev/ttyACM0 flash # adjust port
```

Toolchain: [ESP-IDF v5.5.4](https://docs.espressif.com/projects/esp-idf/en/v5.5.4/esp32s3/index.html),
target **esp32s3**.

Source layout: first-party app code is tracked under [`main/`](main/); the
vendored firmware core lives at
[`vendor/stackchan-esp32/`](vendor/stackchan-esp32/) (lineage in its
[VENDOR.md](vendor/stackchan-esp32/VENDOR.md)); component libraries listed in
[`repos.json`](repos.json) are fetched by `fetch_repos.py` and stay untracked.
