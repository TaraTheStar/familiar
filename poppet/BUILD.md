# Building the `local-only` firmware

This branch (`local-only`) is scoped for a self-hosted, LAN-only StackChan:
all m5stack/tenclass cloud code is removed, and the OTA server + NTP servers
are empty-by-default Kconfig options you must point at your own infrastructure.
See the commit log on this branch for exactly what was changed vs `brett/dotty`.

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

Clones the deps in `repos.json` (incl. `78/xiaozhi-esp32 @ v2.2.4`) into
`firmware/` and applies `patches/xiaozhi-esp32.patch`. These dirs are
gitignored — they are not part of this repo.

```bash
cd firmware
python3 ./fetch_repos.py
```

Expect the line: `Applied patch .../patches/xiaozhi-esp32.patch to .../xiaozhi-esp32`.
If you instead see `cannot be applied cleanly ... skipped`, the build would
silently produce **stock unpatched xiaozhi** — do not proceed; the patch must
apply (this branch already fixed the v2.2.4 context drift that caused that).

## 3. Configure (set YOUR server — required, no cloud defaults)

```bash
cd firmware
idf.py set-target esp32s3
idf.py menuconfig
```

Under **Xiaozhi Assistant**, set both (they are intentionally blank):

- **Default OTA URL** — your self-hosted server, e.g.
  `http://stackchan-server.lan:8003/xiaozhi/ota/`
  (the device gets its websocket/server address from this OTA response).
- **NTP server 1 (primary)** — your LAN NTP host, e.g. `ntp.lan` or `192.168.x.x`.
  Leave blank only if the coin cell keeps RTC time and you use plain `http://`
  for the server (no TLS clock dependency). NTP 2/3 are optional fallbacks.

Behavior if left blank (verified against xiaozhi-esp32 v2.2.4): the device
boots, the OTA check is rejected before any socket opens (no cloud fallback,
no egress), it retries ~10× with a `cloud_slash` alert, then gives up and
runs idle. Fail-safe — but it has no AI server until you configure one.

## 4. Build / flash / monitor

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
