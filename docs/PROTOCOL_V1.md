# StackChan Protocol v1 (Retired — historical reference)

**REMOVED FROM THE SERVER (2026-05-30).** This documents the v1 wire format the
firmware originally spoke; it is retained for historical reference only. The live
protocol is [PROTOCOL_V2.md](PROTOCOL_V2.md). The server no longer implements v1 at
all — the v1 wire types, the MCP-over-WS tool path, and the legacy `/xiaozhi/` route
mounts were deleted in migration Phase 5 (PROTOCOL_V2 §10.2). A device must speak v2
on `/grimoire/`.

This was the wire contract between the StackChan device ([poppet](../poppet)) and
the self-hosted backend ([grimoire](../grimoire)): every HTTP endpoint and
WebSocket message the firmware expected or emitted.

Source of truth at the time (device side) was the patched xiaozhi-esp32 tree the
old fetch-and-patch workflow produced. That workflow is gone: the fork now lives
in-tree at `poppet/vendor/stackchan-esp32/main/`
(`protocols/{protocol,websocket_protocol}.{h,cc}`, `application.cc`, `ota.cc`,
`mcp_server.cc`, `audio/audio_service.{h,cc}`), and its protocol layer speaks
**v2** — so the code no longer matches this document anywhere.

---

## 1. Transports

The device speaks two things over TCP/IP to the server:

1. **One HTTP endpoint** — OTA / discovery (the device calls it `ota_url`, but its real job
   is to hand back the WebSocket URL, server-side time, and optionally a firmware-upgrade
   URL).
2. **One WebSocket connection** — voice + control + MCP, multiplexed.

That's it. No MQTT in this deployment (MQTT path exists in upstream firmware but is not
configured in `boards/.../config.h` — `CONFIG_CONNECTION_TYPE_WEBSOCKET=y`).

---

## 2. HTTP: OTA / discovery endpoint

### 2.1 Request

`POST <ota_url>` or `GET <ota_url>` (POST when the device has a payload to send, which it
always does for `CheckVersion`).

`ota_url` is whatever's flashed into NVS / Kconfig (`CONFIG_OTA_URL`).

**Required headers:**

| Header | Value |
|---|---|
| `Activation-Version` | `"1"` (no eFuse serial) or `"2"` (with eFuse serial) |
| `Device-Id` | MAC address, lowercase colon-separated (`aa:bb:cc:dd:ee:ff`) |
| `Client-Id` | UUID — board-generated, persisted in NVS |
| `User-Agent` | `<board>/<app-version>` |
| `Accept-Language` | `Lang::CODE` (project-configurable; currently `en-US`) |
| `Content-Type` | `application/json` |
| `Serial-Number` | (only if eFuse holds one) |

**Body:** the device's `GetSystemInfoJson()` — a JSON blob with `version, language,
flash_size, mac_address, uuid, chip_model_name, chip_info{}, application{}, partition_table[],
ota{}, board{}`. Server can ignore everything except optionally logging it for inventory.

### 2.2 Response (200 OK, JSON)

All four top-level fields are optional but each has different consequences:

```jsonc
{
  "websocket": {            // REQUIRED in practice — without it the device has no WS URL
    "url": "ws://192.0.2.10:9098/xiaozhi/v1/",
    "token": "optional-bearer-token",  // optional
    "version": 1            // optional; selects binary framing (1 = raw, 2/3 = framed)
  },
  "server_time": {          // optional but recommended (sets device clock)
    "timestamp": 1716608284000,    // unix ms
    "timezone_offset": -300        // minutes from UTC
  },
  "firmware": {             // REQUIRED but can describe the current version (no-op)
    "version": "1.3.1",
    "url": "http://192.0.2.10:9099/firmware/stack-chan.bin",
    "force": 0              // optional; 1 forces re-flash even if version not newer
  },
  "activation": {           // optional, used for cloud onboarding — leave it out
    "code": "...", "message": "...", "challenge": "...", "timeout_ms": 30000
  },
  "mqtt": { ... }           // optional, unused in this deployment
}
```

For a self-hosted server, **return only `websocket`, `server_time`, and `firmware` with
the current version** (so the device thinks it's up to date and proceeds).

### 2.3 Activation endpoint (optional)

`POST <ota_url>/activate` — only called if the OTA response includes `activation`.
Body is `{"algorithm":"hmac-sha256","serial_number":"...","challenge":"...","hmac":"..."}`.
Returns 200 on success or 202 to mean "still waiting." A self-hosted server should never
return an `activation` block, so this endpoint is dead code.

### 2.4 Firmware blob

`GET <firmware.url>` — plain HTTP byte stream, served as the raw `.bin` file. Server needs
to set `Content-Length`. The device streams it in 4KB pages straight to OTA partition.
Only used when `firmware.version` > device's current version (or `force: 1`).

---

## 3. WebSocket: handshake

### 3.1 Connect

Device opens a WS connection to the URL returned in `websocket.url`. Sends as HTTP
upgrade headers:

| Header | Value |
|---|---|
| `Authorization` | `Bearer <token>` (only if token was provided) |
| `Protocol-Version` | `"1"` (default) — also accepts 2 or 3, see §6 |
| `Device-Id` | MAC |
| `Client-Id` | UUID |

### 3.2 Client hello (device → server, immediately after WS open)

```json
{
  "type": "hello",
  "version": 1,
  "transport": "websocket",
  "features": { "mcp": true, "aec": false },
  "audio_params": {
    "format": "opus",
    "sample_rate": 16000,
    "channels": 1,
    "frame_duration": 60
  }
}
```

`aec` is only `true` if firmware built with `CONFIG_USE_SERVER_AEC` (not set in this build).

### 3.3 Server hello (server → device, must arrive within 10 seconds)

```json
{
  "type": "hello",
  "transport": "websocket",
  "session_id": "abc-123-...",
  "audio_params": {
    "sample_rate": 24000,
    "frame_duration": 60
  }
}
```

- `transport` MUST equal `"websocket"` — anything else and the device errors out (line 232
  of websocket_protocol.cc).
- `session_id` — opaque string; device echoes it back in every outgoing JSON message.
- `audio_params.sample_rate` — the **server's TTS output rate**. Device's Opus decoder is
  configured to this. Default 24000 if omitted.
- `audio_params.frame_duration` — server's Opus frame size in ms. Default 60.

If this message doesn't arrive in 10s, device drops the connection.

### 3.4 Channel timeout

Device watchdog: if no incoming WS frame (JSON or binary) for **120 seconds**, the device
treats the channel as dead and reconnects. Send a `tts` heartbeat or anything periodically
if you'll be idle that long.

### 3.5 Close

Device closes by destroying the socket. Server can close cleanly with a WS close frame —
device's `OnAudioChannelClosed` callback then transitions state to `idle`.

---

## 4. WebSocket: device → server (outgoing)

All JSON messages from device include `"session_id": "<from server hello>"`.

### 4.1 `listen` (start/stop/detect listening windows)

Sent when the device starts capturing user audio (or hears the wake word).

```jsonc
// Start listening — server should now expect audio frames
{ "session_id": "...", "type": "listen", "state": "start",
  "mode": "auto" | "manual" | "realtime" }

// Stop listening
{ "session_id": "...", "type": "listen", "state": "stop" }

// Wake-word fired (only sent if firmware built with CONFIG_SEND_WAKE_WORD_DATA;
// in this build that's currently OFF — but the message is in the protocol contract
// and we may flip it on later)
{ "session_id": "...", "type": "listen", "state": "detect", "text": "Hi,ESP" }
```

**Listening modes:**

- `auto` — device's VAD decides when the utterance ends and sends `state:stop`. Server
  should also run VAD as a safety net.
- `manual` — server tells the device when to stop (by sending `tts:start`).
- `realtime` — full duplex (requires AEC); device keeps streaming + plays simultaneously.

### 4.2 `abort` (barge-in)

Sent when the user interrupts (wake word during TTS, or user button).

```json
{ "session_id": "...", "type": "abort", "reason": "wake_word_detected" }
```

`reason` is optional; absence means "general abort" (e.g. user pressed a button).

Server response: stop streaming current TTS immediately, drop pending audio, prepare for
new user turn.

### 4.3 `mcp` (tool call response)

JSON-RPC 2.0 envelope, used by both directions. See §6.

```json
{ "session_id": "...", "type": "mcp", "payload": { /* JSON-RPC */ } }
```

### 4.4 `event` (ambient perception — dotty-branch addition)

Used by this firmware to push out-of-band events ("face seen", "head touched", etc.). The
server can ignore these or surface them to the LLM.

```json
{ "session_id": "...", "type": "event", "name": "face_seen",
  "data": { /* event-specific payload */ } }
```

Note: this is the message type that *lazy-opens* the WS from idle in this firmware (the
bug we already fixed). Server must accept and discard these without error even before any
listen/audio frame has been sent.

### 4.5 Binary frames (Opus audio, device microphone)

Every binary WS frame from device is one Opus packet (or a length-prefixed wrapper around
one). Framing variant chosen by `Protocol-Version` HTTP header at connect time:

- **v1** (default in this build): raw Opus bytes, no header. Frame duration = 60ms,
  sample rate = 16000, channels = 1.
- **v2**: `BinaryProtocol2` (16-byte header, big-endian: `version, type, reserved,
  timestamp, payload_size, payload[]`). Used for server-side AEC echo cancellation.
- **v3**: `BinaryProtocol3` (4-byte header: `type, reserved, payload_size(big-endian),
  payload[]`).

We're on **v1**. Server just demuxes binary frames → Opus decoder.

---

## 5. WebSocket: server → device (incoming)

The device's dispatcher is `application.cc:539-628`. It handles exactly the following
types — anything else is logged as "Unknown message type" and dropped.

### 5.1 `hello` — see §3.3 (only handled inside the WS protocol layer)

### 5.2 `tts` — streaming TTS lifecycle

This is the workhorse. The server emits a sequence of these to drive the speaking state:

```jsonc
// Start of a TTS response (resets the abort flag, server can now stream audio)
{ "type": "tts", "state": "start" }

// One per sentence/chunk — drives the chat-bubble display and ensures device is in
// kDeviceStateSpeaking
{ "type": "tts", "state": "sentence_start", "text": "Hi there, how can I help?" }

// End of the response — device transitions back to listening (or idle if manual mode)
{ "type": "tts", "state": "stop" }
```

Between `start` and `stop`, the server streams Opus binary frames (server's
sample_rate/frame_duration) that the device decodes and plays. The device expects audio
frames to ARRIVE during this window — it doesn't error if they don't, but stays silent.

> **Invariant (learned in implementation):** a multi-sentence reply is ONE
> `start` … `stop` pair, with one `sentence_start` per sentence in between — **not**
> a start/stop per sentence. The device's Speaking state is coupled to this lifecycle:
> a `stop` flips it Speaking→Listening (re-opening the mic), and a following `start`
> races that transition and truncates the next sentence (the clipped-punchline bug).
> grimoire holds a single Speaking session per reply for this reason. This coupling is
> precisely the v1 wart PROTOCOL_V2 §1 fixes by separating `audio_begin`/`audio_end`
> from `caption`. See `grimoire/internal/session/speak.go`.

### 5.3 `stt` — what the ASR heard

```json
{ "type": "stt", "text": "what time is it" }
```

Display-only on the device. Triggers the "thinking" emotion on the avatar.

### 5.4 `llm` — emotion tag

```json
{ "type": "llm", "emotion": "happy" }
```

Sets the avatar's facial expression. Emotion strings the firmware accepts:
`neutral, happy, sad, angry, surprised, thinking, confused, sleeping, ...` (see
`StackChanAvatarDisplay::SetEmotion`).

### 5.5 `mcp` — tool call request — see §6

```json
{ "type": "mcp", "payload": { /* JSON-RPC */ } }
```

### 5.6 `system` — system command

Only `reboot` is implemented:

```json
{ "type": "system", "command": "reboot" }
```

The device reboots immediately (used post-OTA). **Do not send this from a voice exit
handler** — clean session close = WS close, not `system/reboot`.

### 5.7 `alert` — full-screen popup with sound

```json
{ "type": "alert", "status": "Warning",
  "message": "Battery low",
  "emotion": "sad" }
```

All three fields required.

### 5.8 `custom` — only if firmware built with `CONFIG_RECEIVE_CUSTOM_MESSAGE`

Echoed to the chat-message panel as system text. Not enabled in this build. Skip.

---

## 6. MCP-over-WS

The device acts as the **MCP server**, the server acts as the **MCP client**. Both speak
JSON-RPC 2.0 wrapped in `{"type":"mcp","payload": ...}` WS messages. Spec:
`modelcontextprotocol.io/specification/2024-11-05`.

### 6.1 Handshake (server initiates)

```json
// server → device
{ "type": "mcp", "payload": {
    "jsonrpc": "2.0", "id": 1, "method": "initialize",
    "params": { "capabilities": { "vision": { "url": "http://server/vision",
                                              "token": "..." } } }
}}

// device → server
{ "type": "mcp", "payload": {
    "jsonrpc": "2.0", "id": 1,
    "result": { "protocolVersion": "2024-11-05",
                "capabilities": { "tools": {} },
                "serverInfo": { "name": "m5stack-stack-chan", "version": "1.3.1" } }
}}
```

The `capabilities.vision.url` is where the device POSTs JPEGs for "explain this photo"
calls. If you're not running a VLM, omit it.

### 6.2 Tool listing (server → device)

```json
// server → device
{ "type": "mcp", "payload": {
    "jsonrpc": "2.0", "id": 2, "method": "tools/list",
    "params": { "cursor": "", "withUserTools": false }
}}

// device → server: {"tools":[...]} — cursor-paginated, ~8KB chunks
```

Pagination via `cursor`: response includes `nextCursor` if more tools remain. Server
keeps calling `tools/list` with the cursor until exhausted.

### 6.3 Tool call (server → device)

```json
{ "type": "mcp", "payload": {
    "jsonrpc": "2.0", "id": 3, "method": "tools/call",
    "params": { "name": "self.audio_speaker.set_volume",
                "arguments": { "volume": 60 } }
}}
```

Response is JSON-RPC `result` with the tool's return value, or `error` with `message`.

### 6.4 Notifications

Any `method` starting with `notifications` is silently dropped by the device. Server can
fire and forget.

### 6.5 Tools exposed by this firmware

(captured from boot log)

```
self.get_device_status
self.audio_speaker.set_volume
self.screen.set_brightness
self.screen.set_theme
self.camera.take_photo
self.get_system_info          [user-only]
self.reboot                   [user-only]
self.upgrade_firmware         [user-only]
self.screen.get_info          [user-only]
self.screen.snapshot          [user-only]
self.screen.preview_image     [user-only]
self.assets.set_download_url  [user-only]
self.robot.get_head_angles
self.robot.set_head_angles
self.robot.set_led_color
self.robot.set_led_multi
self.robot.set_state
self.robot.set_toggle
self.robot.set_face_identified
self.robot.create_reminder
self.robot.get_reminders
self.robot.stop_reminder
```

`[user-only]` means only listed when `tools/list` is called with `withUserTools: true`.
The LLM should normally see only the non-user-only list — the user-only ones are
admin-style operations.

---

## 7. Session flow (the happy path, end-to-end)

```
1. Boot → POST <ota_url>  → server returns websocket.url + firmware.version=current
2. Device opens WS         → device sends client hello
3. Server sends hello      → device receives session_id, server sample rate
4. Server sends MCP        → initialize → tools/list (paginated)

—idle—

5. User says "Hi,ESP"      → device sends listen{state:start, mode:auto} + Opus frames
6. Server runs VAD/ASR     → server sends stt{text:"hi there"}
7. Server queries LLM      → server sends llm{emotion:"happy"}
8. Server begins TTS       → server sends tts{state:start}
                              tts{state:sentence_start, text:"Hi! How can I help?"}
                              <binary Opus frames>
                              tts{state:stop}
9. Device → listening again (or idle if manual mode)
   GOTO 5
```

Tool call inserts between 7 and 8:
```
7a. Server decides to call self.audio_speaker.set_volume
7b. Server sends mcp{tools/call} → device replies mcp{result}
7c. Server feeds result back to LLM, repeats 7 with the tool result.
```

Barge-in:
```
At any point during 8, user says "Hi,ESP" again
→ device sends abort{reason:wake_word_detected}
→ server stops streaming, drops pending audio, returns to 5.
```

---

## 8. What grimoire must (and deliberately does not) implement

**Hard requirements (anything less and the device misbehaves):**

1. `POST <ota_url>` returning `{websocket, server_time, firmware{version,url}}`.
2. WebSocket server on `websocket.url` that:
   - Accepts the client hello within 10s and replies with server hello including
     `transport, session_id, audio_params{sample_rate, frame_duration}`.
   - Demuxes binary frames as raw Opus (v1 framing).
   - Sends Opus binary frames during TTS playback.
   - Sends at least one JSON frame every 120s to avoid channel timeout (any type works).
3. The `tts` lifecycle: `start → sentence_start* → stop` — **one pair per reply**
   (see the invariant in §5.2). The device's state machine depends on these.
4. MCP `initialize` and `tools/list` — required to get the device's tools into the LLM
   prompt. (Skipping these means the LLM has no idea about volume, camera, robot motion.)

**Soft requirements (implemented, highly desirable):**

5. `stt` frame on ASR result (drives "thinking" emotion).
6. `llm` frame with emotion (drives avatar expression).
7. MCP `tools/call` with proper JSON-RPC envelope.
8. Handle `abort` from device (stop streaming + clear queue).

**Deliberately NOT implemented (out of scope for this deployment):**

- MQTT transport.
- Activation flow (cloud onboarding).
- BinaryProtocol2 / v3 framing.
- `custom` message type.
- Server-side AEC (`CONFIG_USE_SERVER_AEC`) — the device has no AEC; see §9.
- Chinese-language plugins (qweather, ChinaNews, lunar calendar, end_prompt).

---

## 9. Implementation notes (grimoire)

How the live backend behaves within this contract — the non-obvious bits that
took real hardware to shake out:

- **One Speaking session per reply.** See the §5.2 invariant. Driven by the
  device's no-AEC speaking-state coupling.
- **Real-time TTS pacing with a lead.** grimoire paces Opus frames at ~real time,
  kept ~400ms ahead, and drains that lead before sending `tts:stop` so it lands at
  true end-of-audio. With no AEC, a `tts:stop` that arrives early re-opens the mic
  while the speaker is still talking → the device hears itself and clips the tail.
- **Deferred sleep.** A `self.robot.set_state{state:"sleep"}` tool call is held
  until after the farewell finishes speaking; dialogue is cleared after sleep.
- **The AI stack.** ASR = whisper.cpp in-process via cgo (model bundled in the
  image), TTS = Kokoro (HTTP), LLM = llama-swap (OpenAI-compatible HTTP) with a
  native function-calling tool loop. Vision (§6.1 `capabilities.vision.url`) is
  implemented: a `multipart/form-data` JPEG endpoint forwarding to a multimodal LLM.

Operational gotchas still worth remembering:

- **Server-side VAD safety net.** In `auto` mode the device sends `listen:stop`
  via its own VAD, but grimoire also runs endpoint VAD on incoming Opus for noisy
  rooms — don't rely on the device alone.
- **Timezone offset is in *minutes* from UTC, signed** (`server_time.timezone_offset`).
  Easy to get wrong.
- **Wake-word audio is disabled** (`CONFIG_SEND_WAKE_WORD_DATA=n`). If flipped on,
  the server must accept ~35 Opus frames of pre-roll before the first utterance.
- **MCP pagination.** ~22 tools at ~300 B each fit one 8KB `tools/list` response;
  no pagination needed until the tool set grows a lot.

---

## 10. History

This document began as a reverse-engineering audit of the xiaozhi-esp32 firmware,
written to decide whether to replace the ~15K-line Python xiaozhi-esp32-server
with a focused self-hosted backend. The answer was yes; that backend is
[grimoire](../grimoire) (Go + whisper.cpp), and it implements the contract above.
Earlier revisions of this file carried a "minimum-viable replacement" checklist,
a Python/aiohttp effort estimate, and a "should we rewrite?" recommendation —
dropped now that the rewrite has shipped and is verified on hardware.
