# StackChan Protocol v2 (Draft)

A normative spec for a redesigned wire protocol between a self-hosted StackChan server
and its devices. Fixes the warts in v1 (the live contract, documented in
[PROTOCOL_V1.md](PROTOCOL_V1.md)) while preserving its sensible design choices.

**Status:** Draft. Not yet implemented in firmware or server.

**Selected by:** HTTP header `Protocol-Version: 2` during WS upgrade.

---

## 1. Goals (and non-goals)

**Fix the warts.** Specifically:

| v1 wart | v2 fix |
|---|---|
| `session_id` echoed on every device→server message | Dropped — connection is session |
| `listen` type overloaded (start/stop/wake-detect) | Split into `listen_start` / `listen_stop` / `wake` |
| Device's "speaking" state coupled to `tts:sentence_start` | `audio_begin`/`audio_end` separate from `caption` |
| No correlation IDs outside MCP | `id` field on every request/response pair |
| MCP triple-wrapped: `{type:mcp, payload:{jsonrpc:..., method:...}}` | Tool calls are first-class WS messages |
| Three binary framing variants (v1/v2/v3) | One: raw Opus payload |
| `withUserTools` flag baked into protocol | `permission` field on tool descriptors |
| No backpressure / flow control | Explicit credit-based audio flow control |
| Generic `event` type with no schema | Typed `telemetry` events; schema discovery in hello |
| Hello is one-shot | Hello can be re-sent to renegotiate |
| OTA endpoint overloaded (WS discovery + time + firmware + activation) | Split: `/discover`, firmware-update via tool call, time in WS hello |
| No `error` message type | First-class `error` with `code` and `message` |

> The speaking-state coupling row above is no longer hypothetical: it surfaced in
> production as the clipped-second-sentence / dropped-punchline bug. grimoire works
> around it in v1 by holding one Speaking session per reply (PROTOCOL_V1 §5.2);
> the `audio_begin`/`audio_end`-vs-`caption` split here is the real fix.

**Non-goals.**

- Not changing transports. Still HTTP for discovery + WebSocket for session. (HTTP/2,
  WebTransport, QUIC are all overkill for this hardware and rule out broad library reuse.)
- Not changing audio codec. Still Opus. Still 16kHz mic / 24kHz TTS.
- Not changing MCP semantics. Tools, args, results — same model. We're just dropping the
  JSON-RPC envelope and exposing tool ops as first-class WS messages.
- Not changing the embedding of typed JSON in WS text frames + raw audio in WS binary
  frames. That dichotomy is correct.

---

## 2. Transports

### 2.1 HTTP — discovery only

`GET <discover_url>` returns JSON:

```json
{
  "ws_url": "ws://192.0.2.10:9098/v2/",
  "firmware": {                    // optional
    "version": "2.0.0",
    "url": "http://192.0.2.10:9099/firmware/stack-chan-2.0.0.bin"
  }
}
```

Device on boot:
1. `GET <discover_url>`.
2. If `firmware.version` > current and a URL is provided, `GET <firmware.url>` (raw bytes,
   server must set `Content-Length`) and OTA. Reboot.
3. Else: open WS to `ws_url`. See §3.

**Headers:** Same as v1 (`Device-Id`, `Client-Id`, `User-Agent`, `Accept-Language`,
optionally `Authorization`). No `Activation-Version` (no activation in v2).

**Removed from v1's OTA endpoint:**
- `server_time` → moves into WS hello (one fewer round-trip).
- `activation` → cloud onboarding is out of scope for local-first.
- `mqtt` → MQTT not supported.
- `websocket.version` → use `Protocol-Version` header instead.

### 2.2 WebSocket — everything else

URL from `discover.ws_url`. Required headers at upgrade time:

| Header | Value |
|---|---|
| `Protocol-Version` | `"2"` |
| `Device-Id` | MAC address |
| `Client-Id` | UUID |
| `Authorization` | `Bearer <token>` (only if server requires) |

Server MUST reject the upgrade with HTTP 426 (Upgrade Required) if `Protocol-Version` is
not 1 or 2.

### 2.3 Optional: vision callback

`POST <vision_url>` — multipart/form-data with a JPEG. Only used if the server wants to
analyze photos taken by the device. URL is announced in the WS hello (see §3) so it can
move freely. Not part of the discovery payload.

---

## 3. WebSocket: handshake

### 3.1 Client hello (device → server, immediately)

```json
{
  "type": "hello",
  "id": 1,
  "client": {
    "name": "stack-chan",
    "version": "2.0.0",
    "device_id": "aa:bb:cc:dd:ee:ff",
    "uuid": "00000000-0000-0000-0000-000000000000"
  },
  "audio": {
    "in":  { "codec": "opus", "rate": 16000, "channels": 1, "frame_ms": 60 },
    "out": { "codec": "opus", "rate": 24000, "channels": 1, "frame_ms": 60 }
  },
  "features": ["mcp", "wake_word_audio", "camera", "vision_client"],
  "telemetry_events": ["face_seen", "head_touched", "battery_low", "fell_over"]
}
```

`audio.in` is what device sends to server (microphone).
`audio.out` is what device wants to receive (TTS) — server can downsample/reformat if
its TTS uses different params, OR negotiate down in its hello response.

`features` is a list of opt-in capabilities the device supports. Server uses this to
decide whether to send certain message types.

`telemetry_events` declares the events this firmware can emit (see §6.5). Server can
ignore or wire them into the LLM context.

### 3.2 Server hello (server → device, must arrive within 10s)

```json
{
  "type": "hello",
  "id": 1,
  "result": "ok",
  "server": {
    "name": "stackchan-server",
    "version": "2.0.0"
  },
  "audio": {
    "in":  { "rate": 16000, "frame_ms": 60 },     // confirms what server expects
    "out": { "rate": 24000, "frame_ms": 60 }      // confirms what server will send
  },
  "features": ["mcp", "tools", "vision"],
  "time": {
    "unix_ms": 1716608284000,
    "tz_offset_min": -300                          // signed minutes from UTC
  },
  "vision_url": "http://192.0.2.10:9099/vision", // optional
  "flow_control": {
    "audio_credit_initial": 40                     // see §5
  }
}
```

If server can't match an `audio` param the device offered, it MUST either pick a different
value (within both ends' capabilities) or fail the hello:

```json
{ "type": "hello", "id": 1, "error": {
    "code": "UNSUPPORTED_AUDIO",
    "message": "Server requires 16k mic input"
}}
```

The device then closes the connection.

### 3.3 Hello renegotiation

Either side MAY send a fresh `hello` (with a new `id`) mid-session to renegotiate. Typical
use: server wants to switch to a realtime/AEC pipeline. The other side responds with
either a matching `hello` (accept) or `hello` with `error` (reject; old params remain).

### 3.4 Heartbeat

Use WebSocket-native ping/pong frames. Either side SHOULD send a ping every 30s of
inactivity; receiver SHOULD respond within 5s. If three pings go unanswered, close.

This replaces v1's implicit 120s "no incoming JSON = dead channel" rule, which couldn't
distinguish slow servers from dead ones.

### 3.5 Graceful close

WebSocket close frame (code 1000) with reason text optional. Causes the other side to
release session resources cleanly. Device transitions to `idle`.

---

## 4. Message catalog

### 4.1 Conventions

- **`type`** field is always present, snake_case.
- **`id`** is a request-correlation ID, present iff the sender expects a typed response.
  IDs are scoped to the sending direction (device IDs and server IDs are independent
  namespaces). Integer, monotonic per side, starts at 1.
- **Responses** carry the same `type` and `id` as the request, plus either `result` or
  `error`.
- **Notifications** (one-way) carry `type` only — no `id`, no response expected.
- **Errors** use the structure: `{ "error": { "code": "STRING_CODE", "message": "human readable" } }`.

### 4.2 Listen lifecycle (device → server)

**Listen start** — device begins capturing user audio:

```json
{ "type": "listen_start", "mode": "auto" }    // mode: "auto" | "manual" | "realtime"
```

Server should now expect Opus binary frames until `listen_stop` arrives.

**Listen stop** — device-side VAD decided utterance ended (or server told it to stop in
manual mode):

```json
{ "type": "listen_stop" }
```

**Wake fired** — separate notification, independent of listen state:

```json
{ "type": "wake", "phrase": "hi_stackchan", "score": 0.81 }
```

`score` is optional model confidence. `phrase` is the wake-word phrase identifier (not
necessarily a literal display string).

**Abort** — user interrupted server's TTS:

```json
{ "type": "abort", "reason": "wake" }         // or "user_action", "timeout", null
```

### 4.3 Speech recognition (server → device)

**Transcript** — partial or final ASR result:

```json
{ "type": "transcript", "text": "what time is it", "final": true }
```

Display-only on device. Drives the "thinking" emotion.

### 4.4 Audio playback / captions (server → device)

**Audio begin** — server is about to stream Opus frames:

```json
{ "type": "audio_begin", "utterance_id": 42, "estimated_duration_ms": 4500 }
```

`utterance_id` is server-assigned, used to correlate cancel/captions with this audio
stream. `estimated_duration_ms` is optional; lets device pre-allocate.

**Audio end** — server has finished streaming this utterance:

```json
{ "type": "audio_end", "utterance_id": 42 }
```

Device transitions out of `speaking` state on receipt.

**Audio cancel** — server (rarely) wants to cancel an in-flight audio stream it started:

```json
{ "type": "audio_cancel", "utterance_id": 42 }
```

Device flushes the decoder queue for this utterance.

**Caption** — text to display, independent of audio state:

```json
{ "type": "caption", "utterance_id": 42, "text": "Hi there!", "final": false }
{ "type": "caption", "utterance_id": 42, "text": "Hi there! How can I help?", "final": true }
```

`final: true` indicates the last caption update for this utterance. Subsequent captions
with the same `utterance_id` after `final: true` are protocol errors.

Decoupling means: the server CAN send `caption` without any audio (display-only update),
or send audio without caption (silent playback), or mix freely.

### 4.5 Display state (server → device)

```json
{ "type": "display", "emotion": "happy", "status": "speaking" }
```

Replaces v1's `llm{emotion}`. `emotion` and `status` are independently optional; either
or both can be present. Allowed values:

- `emotion`: `neutral, happy, sad, angry, surprised, confused, thinking, sleeping`,
  plus whatever the firmware's avatar supports.
- `status`: `idle, listening, thinking, speaking, error, configuring`.

### 4.6 Alert (server → device)

Full-screen popup with optional sound:

```json
{
  "type": "alert",
  "title": "Battery low",
  "message": "Please charge",
  "emotion": "sad",
  "sound": "vibration"
}
```

`title`, `message`, `emotion` required. `sound` optional; allowed values are firmware-
specific.

### 4.7 System (server → device)

```json
{ "type": "system", "command": "reboot" }
```

Currently only `reboot` is defined. Intended for post-OTA. **Do not send on voice
"goodbye"** — clean session close = WS close frame, not `system/reboot`.

### 4.8 Telemetry (device → server)

Notification, no response:

```json
{ "type": "telemetry", "event": "face_seen",
  "data": { "x": 120, "y": 80, "confidence": 0.92 } }
```

The set of legal `event` names is declared in `hello.telemetry_events`. Server may
register listeners for specific events; unrecognized events SHOULD be logged but not
treated as protocol errors (forward compatibility).

### 4.9 Error (either direction)

```json
{ "type": "error", "code": "ASR_TIMEOUT", "message": "ASR service did not respond in 5s",
  "ref_id": 42 }                  // optional: ID of the request this error relates to
```

`code` is an enumerated machine-readable string. Recipient should not block on error
codes — log + degrade gracefully.

### 4.10 Tool messages (both directions)

See §6.

---

## 5. Audio flow control

v1 had no backpressure. v2 uses **credit-based flow control** for server→device audio.

### 5.1 Initial credits

Server announces initial credit count in its hello (`flow_control.audio_credit_initial`).
This is the number of binary audio frames the server may send before receiving any credit
refills. Recommended: 40 (= 2.4s buffer at 60ms frames).

### 5.2 Credit refill

Device sends:

```json
{ "type": "audio_credit", "frames": 20 }
```

Server increments its credit counter by 20. Server MUST NOT send more binary audio
frames than it has credit for. If credit hits zero, server pauses; resumes on next
`audio_credit`.

### 5.3 Device side

Device sends `audio_credit` as its decoder queue drains. Typical: send `audio_credit:N`
each time N frames are dequeued from the playback buffer.

### 5.4 Why credits, not blocking sends

ESP32's decoder buffer is fixed (40 packets). Without flow control, sustained TTS streams
can overrun and drop frames silently. Credits make overruns impossible by design.

### 5.5 What about device→server audio (microphone)?

No flow control needed in that direction — microphone audio is naturally rate-limited by
the wall clock (one 60ms frame per 60ms). Server side just consumes as it arrives.

---

## 6. Tools

Replaces v1's `{type:mcp, payload:{jsonrpc:..., method:...}}` triple-envelope with
first-class WS messages. Semantically equivalent to MCP — same model of tools, args,
results — but flat on the wire.

### 6.1 Tool descriptor

```json
{
  "name": "self.audio.set_volume",
  "description": "Set speaker volume (0-100)",
  "args_schema": {
    "type": "object",
    "properties": { "volume": { "type": "integer", "minimum": 0, "maximum": 100 } },
    "required": ["volume"]
  },
  "permission": "public"
}
```

`permission` ∈ `{public, user_only, system_only}`. Server-side decides what to expose
to the LLM:
- `public` — always available to LLM.
- `user_only` — exposed only when user explicitly requests admin operations.
- `system_only` — server-internal use only, never shown to LLM.

### 6.2 List tools (server → device)

```json
// request
{ "type": "tool_list", "id": 5, "cursor": null }

// response
{ "type": "tool_list", "id": 5, "result": {
    "tools": [ /* descriptors */ ],
    "next_cursor": null              // or string for pagination
}}
```

Cursor-based pagination preserved from MCP. Server calls until `next_cursor` is null.

### 6.3 Call tool (server → device)

```json
// request
{ "type": "tool_call", "id": 6,
  "name": "self.audio.set_volume",
  "args": { "volume": 60 } }

// success response
{ "type": "tool_call", "id": 6, "result": true }

// error response
{ "type": "tool_call", "id": 6,
  "error": { "code": "OUT_OF_RANGE", "message": "volume must be 0..100" } }
```

### 6.4 Tool announce (device → server, optional optimization)

Device MAY proactively announce its tool list in the hello extension instead of waiting
for `tool_list`:

```json
// in hello.client:
{ "tools_inline": [ /* descriptors */ ] }
```

If present, server SHOULD skip `tool_list` calls. Saves a round-trip on session open.

### 6.5 Reverse direction: server-exposed tools

v2 allows server→device tool listing too. Symmetrical: device can call `tool_list` /
`tool_call` against the server. Use cases: device asks server for "what time is it",
"who is the current user", "fetch from database". Server tools have the same descriptor
shape.

In v1, only device-as-MCP-server existed. v2 is bidirectional. Both sides MAY implement
the server side of tool calls; neither is required to.

---

## 7. Binary frames

All WebSocket binary frames are Opus audio payloads.

- Device→server: microphone audio, rate from `hello.audio.in`.
- Server→device: TTS audio, rate from `hello.audio.out`.

**One Opus packet per WS binary frame. No header.** Frame duration is fixed at the value
negotiated in hello (60ms).

Correlation with audio_begin/end is by **temporal ordering**: server sends `audio_begin`,
then N binary frames, then `audio_end`. Device knows binary frames received between
those two control messages belong to that utterance. Same in reverse for mic audio (frames
between `listen_start` and `listen_stop` belong to the user turn).

No timestamps in the frame (we don't do server-side AEC). No length prefix (WebSocket
frames are already length-delimited). No magic numbers. Just bytes.

---

## 8. Session flow (happy path)

```
1. Device boot
   ── GET /discover ──>
   <── {ws_url, firmware?} ──
   (OTA if firmware.version > current; else continue)

2. Open WS to ws_url with Protocol-Version: 2

3. Device → hello {id:1, client, audio, features, telemetry_events, tools_inline?}
   Server → hello {id:1, result:ok, server, audio, time, vision_url?, flow_control}

4. Server → tool_list {id:1, cursor:null}   // skipped if tools_inline was provided
   Device → tool_list {id:1, result:{tools:[...], next_cursor:null}}

— idle —

5. User says "Hi Stackchan"
   Device → wake {phrase:"hi_stackchan", score:0.81}
   Device → listen_start {mode:auto}
   Device → <binary Opus frames>...
   Device → listen_stop

6. Server runs ASR
   Server → transcript {text:"what time is it", final:true}
   Server → display {emotion:"thinking", status:"thinking"}

7. Server queries LLM, optionally calls tools:
   Server → tool_call {id:2, name:"self.audio.get_status", args:{}}
   Device → tool_call {id:2, result:{volume:60, muted:false}}

8. Server begins TTS:
   Server → display {emotion:"happy", status:"speaking"}
   Server → audio_begin {utterance_id:7, estimated_duration_ms:2100}
   Server → caption {utterance_id:7, text:"It's 7:30 PM.", final:true}
   Server → <binary Opus frames>... (within audio_credit)
   Device → audio_credit {frames:20} (sent as buffer drains)
   Server → audio_end {utterance_id:7}

9. Device → state listening (or idle if mode:manual)
   GOTO 5
```

### Barge-in

```
During step 8, user says "Hi Stackchan" again:
Device → wake {phrase:"hi_stackchan"}
Device → abort {reason:"wake"}
Server → audio_cancel {utterance_id:7}
(Both sides flush; server proceeds as if step 5 just happened.)
```

---

## 9. Error handling

### 9.1 Request/response errors

When a request fails, the response message has the same `type` and `id` as the request,
plus an `error` field instead of `result`:

```json
{ "type": "tool_call", "id": 6,
  "error": { "code": "TOOL_NOT_FOUND", "message": "no such tool 'self.foo'" } }
```

### 9.2 Unsolicited errors

Either side may emit:

```json
{ "type": "error", "code": "ASR_TIMEOUT", "message": "ASR took >5s, dropping turn",
  "ref_id": 42 }
```

`ref_id` is optional — if the error relates to a specific prior request/utterance, set
it to that ID; otherwise omit.

### 9.3 Standard error codes

Receivers SHOULD recognize at least:

| Code | Meaning |
|---|---|
| `UNSUPPORTED_AUDIO` | Hello negotiation failed |
| `TOOL_NOT_FOUND` | tool_call referenced an unknown tool |
| `TOOL_FAILED` | tool_call executed but returned a failure |
| `OUT_OF_RANGE` | An argument was outside valid range |
| `ASR_TIMEOUT` | Server-side ASR timed out |
| `ASR_FAILED` | Server-side ASR errored |
| `LLM_FAILED` | Server-side LLM call errored |
| `TTS_FAILED` | Server-side TTS errored |
| `BUSY` | Receiver can't handle another request right now |
| `INTERNAL` | Catch-all for unexpected errors |

Codes are forward-compatible: new codes can be added. Receivers MUST tolerate unknown
codes (treat as `INTERNAL`).

### 9.4 Protocol violations

If a message violates the protocol (missing required field, invalid type, etc.), receiver
SHOULD send an `error` with code `PROTOCOL_VIOLATION` and `ref_id` of the offending
message, then continue. Don't close the connection over a single bad message.

---

## 10. Migration from v1

Server implements BOTH protocols, selected by `Protocol-Version` header at WS upgrade
(this header already exists in v1 — currently always `"1"`).

| Phase | Server | Firmware |
|---|---|---|
| 0 | v1 only (current) | v1 only (current) |
| 1 | v1 + v2 (parallel impls) | v1 only |
| 2 | v1 + v2 | v2 added, defaulting to v1 |
| 3 | v1 + v2 | v2 default |
| 4 | v2 only | v2 only |

Estimated firmware change to add v2: ~1 week of work (mostly in
`xiaozhi-esp32/main/protocols/` and the dispatcher in `application.cc`). Hardware
drivers, audio pipeline, MCP server logic stay unchanged.

---

## 11. Open questions

1. **Should `wake` carry the wake-word audio?** v1 has `CONFIG_SEND_WAKE_WORD_DATA` that
   pre-sends ~35 Opus packets before the wake message. v2 could pre-define this as part
   of `wake` (binary frames before the `wake` JSON), or keep it optional. Recommend:
   binary frames first, then `wake` JSON, then `listen_start`. Server's ASR gets the wake
   phrase as part of the user turn audio — better recognition.

2. **Should `caption` support segment-level granularity?** Right now it's whole-utterance
   text that can be revised. Some TTS pipelines emit per-word timing. Could add
   `caption {utterance_id, segments: [{text:"Hi", start_ms:0, end_ms:200}]}`. Defer to
   v2.1 unless needed.

3. **Should tools have a `version` field?** For schema evolution. e.g.
   `self.audio.set_volume@v2`. Helps if device firmware changes a tool's args without
   renaming. Not blocking — can add later.

4. **Should there be a session-level `goodbye` exchange?** Currently we rely on WS close.
   A `goodbye` notification before close might be nicer (lets the recipient log the
   reason: idle timeout, user farewell, error, restart).

5. **MCP compatibility shim?** Some tools (Anthropic's MCP servers, Claude Desktop, etc.)
   speak standard MCP. If we want to bridge those into the server, we'd implement an
   MCP-to-v2 adapter server-side. Not part of the protocol itself.

6. **Streaming transcripts?** v1 only sends final `stt`. v2's `transcript` has a `final`
   field, implying partials are allowed. Worth confirming whether server-side ASR emits
   partials (SenseVoiceSmall is batch-only AFAIK; would need streaming ASR for true
   partials). For now `final: true` always.

7. **Should `display` updates be batched?** A sequence of
   `display{status:thinking} → display{emotion:happy} → display{status:speaking}` is
   three messages where one would do. We could allow combined updates (already supported
   by the schema — both fields optional). Should we *require* combined updates? Probably
   not — flexibility wins for streaming UI.

---

## 12. Estimated complexity

Server (Go) implementing v2: ~600 lines. (vs ~800 for v1-only — v2 is actually slightly
simpler because no JSON-RPC wrapper, no session_id juggling, no triple-framing logic.)

Server speaking both v1 + v2: ~1100 lines (the v1 layer is a thin adapter on top of v2's
internals).

Firmware patches to add v2: ~600 lines of C++, concentrated in `protocols/protocol_v2.cc`,
`protocols/websocket_protocol_v2.cc`, and `application.cc` dispatcher additions. Most v1
code stays as-is for backward compat.

---

## 13. Status of this document

Draft. Bake it for a few days, build the server against v1 for short-term needs, revisit
v2 once we've actually used the system and seen what hurts.

When implementing, also write:
- `examples/` — JSON message exemplars for every type, suitable for protocol tests.
- A wire-protocol fuzzer to catch dispatcher bugs.
- A v1↔v2 adapter test harness that proves the migration path.
