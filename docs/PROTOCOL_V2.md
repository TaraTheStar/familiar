# StackChan Protocol v2

The normative spec for the wire protocol between a self-hosted StackChan server
and its devices. Fixes the warts in v1 (the retired predecessor, documented in
[PROTOCOL_V1.md](PROTOCOL_V1.md)) while preserving its sensible design choices.

**Status: LIVE — this is the only protocol the server and firmware speak.** §11 open
questions are resolved (see §11), the server contract is locked, and Phase 5 (§10) is
complete: grimoire is **v2-only** (v1 removed) and the poppet firmware is fully ported
to v2. Per-section "**Server contract**" call-outs pin what the reference server does,
including the few deferred items (hello renegotiation, `tools_inline`). Voice barge-in
is specified (§8) but **deferred to v2.1** — the shipped system is half-duplex (see §8).

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
- Not changing audio codec. Still Opus. 16kHz mic / 16kHz TTS — TTS is dictated at the
  device's native 16k (its playback is AEC-locked to the mic rate), and the server
  downsamples its 24kHz Kokoro source so the device decodes straight to output with no
  on-device resample. (v1 dictated 24kHz and the device resampled, which on this
  CPU-limited board starved playback; see the Stage C bring-up notes.)
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
  "ws_url": "ws://192.0.2.10:9098/grimoire/",
  "firmware": {                    // optional, present only when an update is offered
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

> **Server contract:** grimoire serves v2 discovery at **`GET /discover`** (the lean
> `{ws_url, firmware?}` shape above; `firmware` is present only when an update URL is
> configured; POST is tolerated for OTA-client compatibility). The firmware's boot
> client accepts **both** this shape and the richer v1-era OTA response, so its
> configured URL may point at either `/discover` (v2-native, preferred) or
> `/grimoire/ota/` / `/ota/`, which stay mounted for already-flashed devices. All
> advertise the same `ws_url`.

### 2.2 WebSocket — everything else

URL from `discover.ws_url`. Required headers at upgrade time:

| Header | Value |
|---|---|
| `Protocol-Version` | `"2"` |
| `Device-Id` | MAC address |
| `Client-Id` | UUID |
| `Authorization` | `Bearer <token>` (only if server requires) |

Server MUST reject the upgrade with HTTP 426 (Upgrade Required) unless
`Protocol-Version` is `2` (or absent, which is treated as 2 — the server is v2-only and
a bare client gets the only protocol on offer).

> **Server contract:** grimoire accepts `Protocol-Version: 2` or an absent header and
> 426s anything else — including `1`, since Phase 4 (§10) removed the v1 stack. The
> session handler is mounted at **`/grimoire/`**; the old `/xiaozhi/` mounts were
> dropped with the v1 removal (devices are reflashed to dial `/grimoire/` before the
> server is upgraded).

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
    "out": { "codec": "opus", "rate": 16000, "channels": 1, "frame_ms": 60 }
  },
  "features": ["tools", "wake_word_audio", "camera", "vision_client"],
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
    "out": { "rate": 16000, "frame_ms": 60 }      // server downsamples its 24kHz Kokoro to this
  },
  "features": ["tools", "vision"],
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

**Audio params: the server dictates, it does not negotiate.** The hardware is fixed
(16k mic / 16k TTS), so the server's hello states the params it will use and the device
MUST conform (downsample/reformat as needed) or close. (The server itself downsamples its
24kHz Kokoro output to the dictated rate; the rate is the `-tts-sample-rate` flag,
default 16000.) The server does not inspect the
device's offered `audio` to pick a different value. The `UNSUPPORTED_AUDIO` error code
(§9.3) remains defined for a server that *does* choose to validate, but the reference
server never emits it.

> **Server contract:** grimoire sends fixed `audio.in`/`audio.out` from its config and an
> honest `features` list (`["tools"]`, plus `"vision"` when a vision callback is
> configured) and `server.{name,version}`.

### 3.3 Hello renegotiation

Either side MAY send a fresh `hello` (with a new `id`) mid-session to renegotiate. Typical
use: server wants to switch to a realtime/AEC pipeline. The other side responds with
either a matching `hello` (accept) or `hello` with `error` (reject; old params remain).

> **Server contract:** not yet implemented — grimoire logs a post-handshake `hello` and
> keeps the original params. Renegotiation is reserved for the realtime/AEC work.

### 3.4 Heartbeat

The server closes a connection that has been silent (no frame of any kind) past an idle
timeout, matching the firmware's 120s timeout — half-open sockets are reaped rather than
leaked. WebSocket-native ping/pong MAY also be used by either side but is not required.

> **Server contract:** grimoire uses the idle-timeout mechanism (`ReadIdleTimeout`,
> default 120s) and does not send pings. An earlier draft mandated 30s ping/pong; that is
> downgraded to optional — the idle timeout is the normative liveness rule.

### 3.5 Graceful close

WebSocket close frame (code 1000) with reason text optional. Causes the other side to
release session resources cleanly. Device transitions to `idle`. A side MAY send a
`goodbye` notification (§4.10) just before the close to name the reason; it is advisory
and the close frame alone is sufficient.

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

**Wake-word audio (the pre-roll).** If the device advertised the `wake_word_audio`
feature in its hello, a `wake` sent **from idle** opens the turn's audio window: the
device then streams the buffered pre-roll (the ~2s of mic audio captured *before* the
wake fired, so ASR sees the start of the utterance) as binary frames, then sends
`listen_start`, then the live frames, then `listen_stop`. The ordering is:

```
wake {phrase, score}      // opens the audio window (wake_word_audio feature only)
<N binary Opus frames>    // buffered pre-roll — no count field; self-delimited by listen_stop
listen_start {mode}
<live binary Opus frames>
listen_stop
```

The window-opening rule (see §7): with `wake_word_audio`, the turn's audio window
opens at `wake`; **without** it, the window opens at `listen_start` and `wake` carries
no pre-roll. Either way, everything from the window-opening message through
`listen_stop` is one utterance. This resolves §11 Q1 (the spec previously suggested
frames *before* `wake`, which would orphan them under §7's correlation rule).

A `wake` sent **during server TTS** is a barge-in (see §8 — voice barge-in is deferred
to v2.1; the shipped firmware is half-duplex), not a turn start: it is followed by
`abort`, and does **not** open a capture window. Pre-roll applies only to a `wake` from
idle.

**Abort** — user interrupted server's TTS:

```json
{ "type": "abort", "reason": "wake" }         // or "user_action", "timeout", null
```

> **Server contract:** grimoire acts on `abort` (cancels the in-flight reply; the open
> utterance ends with `audio_cancel`, §4.4). It also honors `wake_word_audio` pre-roll: a
> device that advertised the feature gets its audio window opened at a `wake` from idle, so
> pre-roll binary frames sent before `listen_start` are buffered as turn audio (then
> `listen_start` attaches the mode's endpoint detector and keeps the buffered pre-roll; the
> server-VAD does not run over the pre-roll, so the wake tail can't self-endpoint). The firmware
> ships with the feature on (`CONFIG_SEND_WAKE_WORD_DATA=y`, its Kconfig default) and advertises
> `wake_word_audio` iff that option is compiled in; a device that doesn't advertise it, or a
> `wake` mid-window (a barge-in), gets the window at `listen_start` as before.

### 4.3 Speech recognition (server → device)

**Transcript** — partial or final ASR result:

```json
{ "type": "transcript", "text": "what time is it", "final": true }
```

Display-only on device. Drives the "thinking" emotion.

`final: false` is an **incremental partial** emitted while the user is still
speaking; `final: true` is the authoritative result for the turn. The device
shows the latest `text` and may treat `final: true` as "transcript settled."

> **Server contract:** *honored (opt-in).* When streaming is enabled
> (`-asr-streaming`), grimoire periodically re-transcribes the growing mic buffer
> during the listen window and sends `final: false` partials, then exactly one
> `final: true` at turn end. whisper.cpp is batch, so a partial is a full
> re-inference of the buffer-so-far (one in flight at a time, ~700ms debounce);
> a partial whose inference finishes after the turn has fired is dropped, never
> sent after the `final: true`. With streaming off (the default) the server
> sends only the single `final: true` transcript per turn.

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

**Audio cancel** — server wants to cancel an in-flight audio stream it started:

```json
{ "type": "audio_cancel", "utterance_id": 42 }
```

Device flushes the decoder queue for this utterance.

> **Server contract:** grimoire emits `audio_cancel` (instead of `audio_end`) when a turn
> is interrupted — a device `abort` (barge-in, §4.2/§8) cancels the in-flight reply and
> the open utterance ends with `audio_cancel`. A normally-completed reply ends with
> `audio_end`. The two are mutually exclusive per utterance.

**Caption** — text to display, independent of audio state:

```json
{ "type": "caption", "utterance_id": 42, "text": "Hi there!", "final": false }
{ "type": "caption", "utterance_id": 42, "text": "Hi there! How can I help?", "final": false }
{ "type": "caption", "utterance_id": 42, "final": true }
```

`final: true` marks the last caption for this utterance. The terminal caption **omits
`text`** — it is a pure completion marker, not a text update. The device keeps the text it
last displayed and treats `final: true` as "caption complete." Subsequent captions with
the same `utterance_id` after `final: true` are protocol errors. (A `final: true` frame
that *does* carry `text` is still legal for receivers to handle — show it — but the
reference server never emits one.)

**Granularity and text semantics (resolves §11 Q2).** One `caption` per spoken sentence,
all sharing the utterance's `utterance_id`, each emitted in sync with that sentence's
audio. `text` is **cumulative** — the full caption so far, not the latest sentence's delta
— so the device displays `text` verbatim with no accumulation logic of its own (see the
example: each message repeats all prior sentences). The completion marker (`final: true`,
no `text`) follows the last sentence.

Per-word / per-segment timing (`segments: [{text, start_ms, end_ms}]`) is **reserved for
v2.1** and not part of v2: current TTS pipelines (e.g. Kokoro) emit sentence-chunked
audio with no word timing. Adding `segments` later is a new optional field — non-breaking.

> **Server contract:** grimoire emits one `caption` per sentence with `final: false` and
> cumulative `text` (in sync with the audio), then a terminal `caption` with `final: true`
> and **no `text`**. The full text rides the last `final: false` caption; the terminal
> frame only signals completion, so no sentence text is ever duplicated. The device shows
> the latest non-empty `text` and uses `final: true` as the uniform "caption complete"
> signal.

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

> **Server contract:** grimoire logs every telemetry event and acts on `battery_low`
> by sending a full-screen `alert` (§4.6) — `{title:"Battery low", emotion:"sad",
> sound:"vibration"}`, with the `data.percent` (if present) folded into the message.
> Unrecognized events are logged and ignored. Feeding telemetry into the LLM's ambient
> context (so the assistant can react in conversation) is future work, not yet wired.

### 4.9 Error (either direction)

```json
{ "type": "error", "code": "ASR_TIMEOUT", "message": "ASR service did not respond in 5s",
  "ref_id": 42 }                  // optional: ID of the request this error relates to
```

`code` is an enumerated machine-readable string. Recipient should not block on error
codes — log + degrade gracefully.

### 4.10 Goodbye (either direction)

Advisory notification sent immediately before a graceful WebSocket close, naming why the
session is ending (resolves §11 Q4):

```json
{ "type": "goodbye", "reason": "user_farewell" }
```

`reason` ∈ `{ idle_timeout, user_farewell, error, restart, shutdown }`. One-way: no `id`,
no response. **Best-effort and advisory only** — it changes nothing about the state
machine; the WebSocket close frame (§3.5) still does the work. Recipients MUST handle a
bare close with no preceding `goodbye` (hard crashes and network drops skip it), and MUST
tolerate unknown `reason` values.

Sent in either direction: server→device (`idle_timeout`, `shutdown`, `restart`),
device→server (`user_farewell`, low battery → `shutdown`). It is **not** `system/reboot`
(§4.7): `goodbye` precedes a clean close; `system{reboot}` is the post-OTA hard restart.
Do not conflate them.

### 4.11 Tool messages (both directions)

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

Credit accounts for **buffer space, not playback**: a server frame that frees its buffer
slot without playing — flushed by a barge-in / `audio_cancel` reset, or dropped because
no utterance was open or the queue was full — MUST be credited back just like a consumed
frame. Otherwise every interruption permanently shrinks the server's send window. Only
server-sent frames mint credit; locally generated audio sharing the same buffer (e.g.
notification sounds) MUST NOT.

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

> **"tools" vs "MCP" — what actually changed.** v2 drops MCP's *wire envelope* (the
> JSON-RPC framing) and *name*, but keeps MCP's *model*: `tool_list` still paginates by
> cursor like `tools/list`, `tool_call` still takes name+args and returns a result like
> `tools/call`, and descriptors keep args schemas + permissions. "tools" is not a new
> system replacing MCP — it is MCP semantics with the envelope peeled off. Three
> consequences worth stating once:
> - The token is **`tools`** everywhere on the v2 wire (hello feature, message types);
>   the word `mcp` does not appear.
> - The device's *internal* tool registry can stay MCP-shaped — only its protocol/
>   transport layer changes to emit these flat messages (see §10).
> - Bridging *external, standard* MCP servers (Claude Desktop, Anthropic's servers) into
>   the catalog is a separate future server-side adapter, not part of this wire (§11 Q5).

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

Tool descriptors carry **no `version` field** (resolves §11 Q3): the descriptor set is
rediscovered every session (via `tool_list` or `tools_inline`), so the advertised schema
is always authoritative within a session — there is no cross-version skew to reconcile.
Because `name` is an opaque string, a future `name@v2` convention can be layered on with
no protocol change if a tool's args ever need to evolve while old firmware is still in the
field.

`permission` ∈ `{public, user_only, system_only}`. Server-side decides what to expose
to the LLM:
- `public` — always available to LLM.
- `user_only` — exposed only when user explicitly requests admin operations.
- `system_only` — server-internal use only, never shown to LLM.

> **Server contract:** grimoire exposes only `public` (and unset, treated as public)
> tools to the LLM; `user_only` and `system_only` are discovered but filtered out before
> the catalog reaches the model. (v1 MCP descriptors carry no `permission`; the device
> already filters `user_only` at `tools/list`, so unset = public is correct there too.)
> Runtime promotion of `user_only` for explicit admin requests is not yet implemented.

> **Result text:** a `tool_call` result is raw JSON. grimoire flattens it for the LLM as
> follows: a JSON string yields its value; `true`/`null`/empty yield `"ok"` (setter-style
> success); anything else is passed through as compact JSON.

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

> **Server contract:** honored. grimoire reads `client.tools_inline` from the hello and, when
> it carries ≥1 usable (named) descriptor, registers that catalog and skips `tool_list`
> discovery entirely. Belt-and-suspenders: if the inline list is present but unusable (empty
> after filtering, or all-nameless), the server falls back to `tool_list` rather than run with
> no device tools — it never ends up toolless because the device announced badly. A descriptor
> with a name but a non-public permission is honored as a deliberate "expose nothing" and does
> **not** trigger fallback. Pure optimization: identical observable tool behavior either way.

### 6.5 Reverse direction: server-exposed tools

v2 allows server→device tool listing too. Symmetrical: device can call `tool_list` /
`tool_call` against the server. Use cases: device asks server for "what time is it",
"who is the current user", "fetch from database". Server tools have the same descriptor
shape.

In v1, only device-as-MCP-server existed. v2 is bidirectional. Both sides MAY implement
the server side of tool calls; neither is required to.

> **Server contract:** *LLM-facing only.* grimoire contributes its own tools to the
> **LLM's** catalog — the in-process helpers like `get_current_time`, plus any **external
> standard-MCP servers** bridged in via the `-mcp-config` adapter (`internal/mcptools`; e.g.
> a `fetch` server giving the model internet reach). External tools are namespaced
> `mcp__<server>__<tool>` so they can't collide with the device catalog, and are dispatched
> server-side (never forwarded to the device). A server that fails to connect is logged and
> skipped, never fatal. grimoire still does **not** answer the *device-initiated* direction
> (it does not serve `tool_list`/`tool_call` *from* the device) — that remains a non-goal.

---

## 7. Binary frames

All WebSocket binary frames are Opus audio payloads.

- Device→server: microphone audio, rate from `hello.audio.in`.
- Server→device: TTS audio, rate from `hello.audio.out`.

**One Opus packet per WS binary frame. No header.** Frame duration is fixed at the value
negotiated in hello (60ms).

Correlation with audio_begin/end is by **temporal ordering**: server sends `audio_begin`,
then N binary frames, then `audio_end`. Device knows binary frames received between
those two control messages belong to that utterance. Same in reverse for mic audio: frames
between the turn's window-opening message and `listen_stop` belong to the user turn. The
window opens at `wake` when the device advertised `wake_word_audio` (so the buffered
pre-roll counts as turn audio — §4.2), otherwise at `listen_start`. There are no binary
frames outside an open window; a frame received with no open window is a protocol error.

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
   Device → wake {phrase:"hi_stackchan", score:0.81}   // opens window if wake_word_audio
   Device → <binary Opus pre-roll frames>...           // only if wake_word_audio (§4.2)
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

— session end —

10. Either side ends the session:
    → goodbye {reason:"idle_timeout"}   // advisory, optional (§4.10)
    → WS close frame (1000)
```

### Barge-in

> **DEFERRED TO v2.1 (voice path).** The wire mechanics below are normative and the
> server implements the `abort` → `audio_cancel` exchange, but **voice** barge-in — the
> wake word firing during server TTS — does not ship in v1 of the release: the firmware
> compiles wake-word-detection-during-speaking out (`CONFIG_WAKE_WORD_DETECTION_IN_SPEAKING`,
> default `n`), so the shipped system is half-duplex. What DOES work today is the manual
> abort (button/touch), which sends `abort` without a preceding `wake`; the server keys
> on `abort` alone, so the `wake` line below is optional in practice.

```
During step 8, user says "Hi Stackchan" again (v2.1) — or presses the button (today):
Device → wake {phrase:"hi_stackchan"}     // voice path only, deferred to v2.1
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

> **Server contract:** grimoire emits `PROTOCOL_VIOLATION` (malformed inbound frame),
> `ASR_FAILED` (transcription error), `LLM_FAILED` / `TTS_FAILED` (turn-pipeline error,
> classified from the failure), and `INTERNAL` (unclassifiable turn error). Errors are
> suppressed when the turn was cancelled by a barge-in (the failure is expected, not
> reportable). It does not emit `ASR_TIMEOUT` (whisper is batch, no timeout), `BUSY`, or
> `UNSUPPORTED_AUDIO` (the server dictates audio, §3.2). The device should log+degrade
> on any code, recognized or not.

### 9.4 Protocol violations

If a message violates the protocol (missing required field, invalid type, etc.), receiver
SHOULD send an `error` with code `PROTOCOL_VIOLATION` and `ref_id` of the offending
message, then continue. Don't close the connection over a single bad message.

---

## 10. Migration from v1 (completed)

Historical plan, kept as the record of how the cutover ran. During migration the server
implemented both protocols, selected by the `Protocol-Version` header at WS upgrade.

| Phase | Server | Firmware |
|---|---|---|
| 0 | v1 only | v1 only |
| 1 | v1 + v2 (parallel impls) | v1 only |
| 2 | v1 + v2 | v2 added, defaulting to v1 |
| 3 | v1 + v2 | v2 default |
| 4 | **v2 only** ✅ done 2026-05-30 | **v2 only** ✅ ported (Stage B/C) |

**Phase 4 reached server-side, 2026-05-30.** v1 is removed from the server: the v1 wire types,
`Decode`, the `internal/mcp` package, the v1 `deviceOut`/decoder/tool-port (`wire_v1.go`,
`tools_v1.go`), and the legacy `/xiaozhi/` route mounts are all deleted. The upgrade handler
rejects any `Protocol-Version` other than empty or `2`, and routes mount under `/grimoire/`
only. `package protocol` keeps just `AudioParams` + the OTA HTTP response types;
`docs/PROTOCOL_V1.md` is retained for history. The device is reflashed to dial `/grimoire/`
before reconnecting, so no `/xiaozhi/` compatibility window is kept.

Estimated firmware change to add v2: ~1 week of work (mostly in
`xiaozhi-esp32/main/protocols/` and the dispatcher in `application.cc`). Hardware
drivers, audio pipeline, MCP server logic stay unchanged.

---

## 11. Resolved questions

All seven of the v1 draft's open questions are now decided. The decisions are reflected
in the normative sections above; this section records the outcome and rationale.

1. **Should `wake` carry the wake-word audio? → Yes; `wake` opens the audio window.**
   When the device advertises the `wake_word_audio` feature, a `wake` from idle opens the
   turn's audio window and the buffered pre-roll streams as binary frames before
   `listen_start` (§4.2, §7). This differs from this draft's original suggestion of frames
   *before* `wake`, which would orphan them under §7's correlation rule. Gated by the
   feature flag: the firmware advertises `wake_word_audio` iff it is built with
   `CONFIG_SEND_WAKE_WORD_DATA` (default y), and sends `wake` before the pre-roll frames.

2. **Should `caption` support segment-level granularity? → Not in v2.** One `caption` per
   sentence, cumulative `text`, device displays verbatim (§4.4). Per-word `segments` is
   reserved for v2.1 (a non-breaking optional addition); current TTS has no word timing.

3. **Should tools have a `version` field? → No.** Schema is rediscovered per session, so
   there is no version skew within a session; a future `name@v2` convention needs no
   protocol change (§6.1).

4. **Should there be a session-level `goodbye` exchange? → Yes, advisory.** A best-effort
   `goodbye {reason}` notification in either direction, sent before a graceful close; the
   close frame still does the work (§4.10, §3.5).

5. **MCP compatibility shim? → implemented as a server-side adapter (not a wire change).**
   Bridging external standard-MCP servers into the tool catalog is done off-wire by
   `internal/mcptools` (`-mcp-config`): grimoire connects to the configured servers at
   startup, lists their tools, and exposes them to the **LLM** namespaced
   `mcp__<server>__<tool>`, dispatching calls server-side. The wire protocol is unchanged —
   the device never sees these tools. See §6.5 "Server contract." (Device-initiated server
   tools — the reverse wire direction — remain a non-goal.)

6. **Streaming transcripts? → implemented (opt-in).** whisper.cpp is batch, so streaming is
   done by periodic re-transcription of the growing mic buffer (not token-level streaming):
   with `-asr-streaming` on, the server emits `final: false` partials during the listen
   window, then one `final: true` at turn end. Off by default (each partial is a full
   re-inference). See §4.3 "Server contract." A token-streaming ASR could later drop in
   behind the same wire shape.

7. **Should `display` updates be batched? → Allowed, not required.** Both fields are
   independently optional (§4.5); senders SHOULD combine `emotion` and `status` when both
   are known together, but streaming partial updates remain legal.

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

This is the **live contract**: the §11 open questions are resolved, the server contract
is locked, and per the migration plan (§10, Phase 5) the server is **v2-only** and the
firmware speaks only v2. The wire format is stable; changes from here follow normal
spec-then-implement discipline.

The implementation-companion artifacts (all under `grimoire/internal/protov2` and
`grimoire/internal/session`) are done:
- **Examples** — JSON exemplars for every message type (`protov2/testdata/examples/`,
  generated + validated by `TestExamples`).
- **Wire fuzzer** — `FuzzDecode`: no panics, type-stable round-trips on the dispatcher.
- **Goldens / seam tests** — the seam-contract test pins the loop's call order;
  per-protocol goldens pin exact bytes. (The v1↔v2 equivalence harness retired with v1.)

Deferred (tracked in the "Server contract" call-outs): hello renegotiation (§3.3),
`tools_inline` device-side (§6.4), voice barge-in (§8, v2.1), and per-word caption
segments (§11 Q2, v2.1).
