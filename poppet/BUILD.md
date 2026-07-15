# Building the firmware

The `poppet` firmware is scoped for a self-hosted, LAN-only StackChan: all
m5stack/tenclass cloud code is removed, and the OTA server + NTP servers are
empty-by-default Kconfig options you must point at your own infrastructure. The
firmware core is a pruned in-tree fork under `vendor/stackchan-esp32/` (see
`vendor/stackchan-esp32/VENDOR.md` for upstream provenance); edit it directly.

> The repo's `env/` virtualenv is **not** used for the firmware build. ESP-IDF
> manages its own Python environment. You only need system `git` + `python3`
> plus the ESP-IDF SDK below.

## 1. Install the ESP-IDF SDK (one time)

`idf.py` is **not** a pip package — it ships inside the ESP-IDF SDK. This
firmware is pinned to **ESP-IDF v5.5.4**, target **esp32s3**.

```bash
git clone -b v5.5.4 --recursive https://github.com/espressif/esp-idf.git ~/esp/esp-idf
~/esp/esp-idf/install.sh esp32s3
```

Then, **in every new shell** you build from:

```bash
. ~/esp/esp-idf/export.sh        # adds idf.py to PATH, sets IDF_PATH,
                                 # activates ESP-IDF's own python env
idf.py --version                 # expect: ESP-IDF v5.5.4
```

## 2. Fetch source dependencies (one time, needs network)

The firmware core is **vendored in-tree** at `vendor/stackchan-esp32/` (a pruned
fork of `78/xiaozhi-esp32 @ v2.2.4` — see its `VENDOR.md`); it is committed, not
fetched. `fetch_repos.py` now only clones the unmodified component libraries
(mooncake, mooncake_log, smooth_ui_toolkit, ArduinoJson, esp-now) into
`components/` (gitignored). Run from the `poppet/` dir:

```bash
python3 ./fetch_repos.py
```

> **History:** through 2026-05 the core was fetched fresh and a large patch
> (`patches/xiaozhi-esp32.patch`) was applied on top. Once the project went
> Protocol-v2-only it diverged from upstream for good, so the core was vendored
> and the patch + its `repos.json` entry removed. To pull a newer upstream now,
> do a manual 3-way merge (see `vendor/stackchan-esp32/VENDOR.md`).

## 3. Configure (set YOUR server — required, no cloud defaults)

From the `poppet/` dir:

```bash
idf.py set-target esp32s3
idf.py menuconfig
```

Under the **Xiaozhi Assistant** menu (upstream's Kconfig menu name — unchanged so
the vendored `CONFIG_*` symbols stay stable), set both (they are intentionally
blank):

- **Default OTA URL** — your self-hosted server's discovery endpoint, e.g.
  `http://192.0.2.10:9099/discover`
  (the device gets its websocket/server address from this response —
  PROTOCOL_V2 §2.1). The legacy rich shape at `http://…:9099/grimoire/ota/`
  also still works (the boot client accepts both; the legacy `/xiaozhi/`
  mount is gone). If you use the legacy path, **keep the trailing slash:**
  without it (`…/grimoire/ota`) the server 307-redirects to the slashed path
  and the ESP OTA client does **not** follow redirects, so the check fails.
- **NTP server 1 (primary)** — your LAN NTP host, e.g. `ntp.lan` or `192.168.x.x`.
  Leave blank only if the coin cell keeps RTC time and you use plain `http://`
  for the server (no TLS clock dependency). NTP 2/3 are optional fallbacks.

Behavior if left blank (verified against xiaozhi-esp32 v2.2.4): the device
boots, the OTA check is rejected before any socket opens (no cloud fallback,
no egress), it retries ~10× with a `cloud_slash` alert, then gives up and
runs idle. Fail-safe — but it has no AI server until you configure one.

## 4. Build / flash / monitor

From the `poppet/` dir:

```bash
idf.py build
idf.py -p /dev/ttyACM0 flash      # adjust port (often /dev/ttyUSB0 or /dev/ttyACM0)
idf.py -p /dev/ttyACM0 monitor    # Ctrl-] to exit
```

On first boot, watch `monitor` to confirm OTA-check behavior matches the
description above before relying on the network isolation.

## 5. Network isolation (the actual privacy guarantee)

Firmware scoping is defense-in-depth, not the boundary. Enforce at the network:

- Put the device on an isolated VLAN/SSID with a firewall rule dropping its
  IP → WAN (internet). It then physically cannot leave the LAN.
- Run local DNS so the OTA/server hostname resolves to your LAN box.
- Allow only: device → your server (OTA/websocket) and device → your LAN
  NTP (UDP/123), both LAN-only.

## Notes

- Use `http://` (not `https://`) for the LAN server to avoid a TLS-vs-clock
  dependency, unless you have NTP configured.
- ESP-IDF keeps its Python venv under `~/.espressif/`. Don't install build
  deps into the repo's `env/` — sourcing `export.sh` is all that's needed.
