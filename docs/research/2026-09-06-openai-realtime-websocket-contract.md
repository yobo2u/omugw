# OpenAI Realtime API WebSocket Wire Contract

**Date:** September 6, 2026
**Topic:** The authoritative client↔server wire contract for the OpenAI Realtime API
over WebSocket (GA interface): connect URL and query params, auth headers,
subprotocol negotiation, the complete event taxonomy and JSON envelope, session
lifecycle, audio frame encoding, error shape and close codes, keepalive/idle
timeouts, and usage/token accounting.
**Trigger:** omugw is adding a WebSocket transport layer (existing transports are
HTTP and SSE only) that must proxy OpenAI Realtime API clients. This note is the
first-party contract the transport design must cite.

**Scope:** First-party OpenAI sources only — platform docs (`developers.openai.com`),
the published OpenAPI spec (`openai/openai-openapi`), and official SDK source
(`openai-python`, `openai-node`). No third-party write-ups. Retrieved 2026-09-06.

**Pinned commits.** All spec/SDK line references below are anchored to these SHAs:

| Repo | Commit |
|---|---|
| `openai/openai-openapi` | [`b61ced96`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml) |
| `openai/openai-python` | [`be928151`](https://github.com/openai/openai-python/blob/be928151372e4b62adb4a1571cda52ad759b38be/src/openai/resources/realtime/realtime.py) |
| `openai/openai-node` | [`d8f6be93`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/realtime/websocket.ts) |

## Findings

### 1. Connect URL, query parameters, and auth headers

**URL.** The documented WebSocket endpoint is:

```
wss://api.openai.com/v1/realtime?model=gpt-realtime-2.1
```

This exact URL appears in every official connection sample on the WebSocket guide
(Node `ws`, Python `websocket-client`, browser `WebSocket`) [S2].

**Scheme is enforced by the SDK, not merely conventional.** The GA Node client
rejects any non-`wss:` URL — including a caller-supplied custom URL builder:

```ts
if (this.url.protocol !== 'wss:') {
  throw new OpenAIError('Realtime WebSocket URLs must use the wss: protocol.');
}
```
([`websocket.ts#L206-L208`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/realtime/websocket.ts#L206-L208);
same check for custom builders at
[`internal-base.ts#L278-L280`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/realtime/internal-base.ts#L278-L280))

The Python client derives the URL by swapping the HTTP scheme and appending
`/realtime` to the base path, i.e. `wss://api.openai.com/v1/realtime`
([`realtime.py#L725-L734`](https://github.com/openai/openai-python/blob/be928151372e4b62adb4a1571cda52ad759b38be/src/openai/resources/realtime/realtime.py#L725-L734)).

**Query parameters — exactly one connection target.** The GA Node URL builder
enforces that exactly one of `model`, `callID`, or `intent` is supplied, and maps
them to three mutually exclusive query params:

| Query param | Value | Meaning |
|---|---|---|
| `model` | e.g. `gpt-realtime-2.1` | Start a new model-backed session |
| `intent` | `transcription` (only legal value) | Start a transcription-only session |
| `call_id` | e.g. `rtc_...` | Attach a sideband connection to an existing WebRTC/SIP call |

```ts
if (Number(hasModel) + Number(hasCallID) + Number(hasIntent) !== 1 || ...) {
  throw new Error(
    'Pass exactly one of `model`, `callID`, or transcription `intent` when opening a Realtime WebSocket.',
  );
}
```
([`internal-base.ts#L265-L274`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/realtime/internal-base.ts#L265-L274),
param mapping at [`#L296-L303`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/realtime/internal-base.ts#L296-L303))

The `call_id` form is independently documented in prose:
`wss://api.openai.com/v1/realtime?call_id=rtc_xxxxx` for both WebRTC sideband and
SIP calls; for SIP "the WebSocket connection will live for the life of the SIP
call" [S6].

**Auth headers (server-to-server).** A standard API key is used, because the token
"will only be available on your secure backend server" [S2]:

```
Authorization: Bearer $OPENAI_API_KEY
OpenAI-Safety-Identifier: hashed-user-id     # optional
```

`OpenAI-Safety-Identifier` is recommended but not required; for a trusted-server
WebSocket connection it is set on the connection request itself [S1][S2].

**No beta/version header on the GA interface.** This is an explicit migration
instruction, not an omission: "Remove the `OpenAI-Beta: realtime=v1` header when
calling the GA interface" [S1]. Confirmed in SDK source — the GA Python connect
path sends only `client.auth_headers` plus user-supplied extras, with no
`OpenAI-Beta`
([`realtime.py#L713-L723`](https://github.com/openai/openai-python/blob/be928151372e4b62adb4a1571cda52ad759b38be/src/openai/resources/realtime/realtime.py#L713-L723)).
The **legacy** Node beta path still hardcodes `'OpenAI-Beta': 'realtime=v1'`
([`beta/realtime/ws.ts#L75`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/beta/realtime/ws.ts#L75)) —
useful as a compatibility signal for inbound clients, but not the GA contract.

### 2. Subprotocol negotiation

Two distinct connection modes, and the difference is the whole story for a proxy:

**(a) Header auth — no subprotocol needed.** When request headers are available
(Node `ws`, Python, any server-side client), auth goes in the `Authorization`
header and no subprotocol is negotiated [S2].

**(b) Subprotocol auth — for environments that cannot set headers.** Browsers
cannot set headers on `WebSocket`, so credentials are smuggled through
`Sec-WebSocket-Protocol`. The documented browser form [S2]:

```javascript
const ws = new WebSocket(
  "wss://api.openai.com/v1/realtime?model=gpt-realtime-2.1",
  [
    "realtime",
    // Use a short-lived token fetched from your application server.
    "openai-insecure-api-key." + OPENAI_REALTIME_EPHEMERAL_KEY,
    // Optional
    "openai-organization." + OPENAI_ORG_ID,
    "openai-project." + OPENAI_PROJECT_ID,
  ]
);
```

The GA Node SDK constructs exactly two of these tokens:

```ts
const protocols = ['realtime', ...(azure ? [] : [`openai-insecure-api-key.${client.apiKey}`])];
```
([`websocket.ts#L212`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/realtime/websocket.ts#L212))

**The GA/beta difference is load-bearing.** The legacy beta path appends a third
token, `'openai-beta.realtime-v1'`:

```ts
const protocols = [
  'realtime',
  ...(azure ? [] : [`openai-insecure-api-key.${client.apiKey}`]),
  'openai-beta.realtime-v1',
];
```
([`beta/realtime/websocket.ts#L216-L220`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/beta/realtime/websocket.ts#L216-L220))

So the subprotocol vocabulary is: `realtime` (always first),
`openai-insecure-api-key.<KEY>`, `openai-organization.<ORG>`,
`openai-project.<PROJ>`, and — beta only — `openai-beta.realtime-v1`.

**Not documented:** which subprotocol the server *selects* in its
`Sec-WebSocket-Protocol` response header. No first-party source states the
negotiated value (§9.1).

### 3. Event taxonomy and JSON envelope

**Envelope.** Every frame in both directions is a **JSON object serialized as a
WebSocket text message**, discriminated by a required string `type` field. The
spec declares both unions with `discriminator: propertyName: type`
([client](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L50009-L50025),
[server](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L51870-L51928)).
The docs state it plainly: "you will both send and receive JSON-serialized events
as strings of text" [S2].

Envelope rules that differ by direction:

| Field | Client→server | Server→client |
|---|---|---|
| `type` | required | required |
| `event_id` | **optional**, client-generated, `maxLength: 512`. "It will be passed back if there is an error with the event, but the corresponding `session.updated` event will not include it." | **required** — "The unique ID of the server event." |

(client `event_id` at [`openapi.yaml#L50046-L50049`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L50046-L50049);
server `event_id` required at [`#L51954-L51957`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L51954-L51957))

The GA Node parser hard-fails a frame that is not a JSON object carrying a string
`type` — "Realtime WebSocket event must be an object with a string type."
([`websocket.ts#L223-L239`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/realtime/websocket.ts#L223-L239)).

#### 3.1 Client→server events (11 total)

Enumerated from the `RealtimeClientEvent` union
([`openapi.yaml#L50009-L50025`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L50009-L50025)):

| `type` | Purpose |
|---|---|
| `session.update` | Update session config; server replies `session.updated` |
| `input_audio_buffer.append` | Append base64 audio to input buffer (**no server ack**) |
| `input_audio_buffer.commit` | Commit buffer → creates a user message item |
| `input_audio_buffer.clear` | Clear the input buffer |
| `output_audio_buffer.clear` | Clear the output audio buffer |
| `conversation.item.create` | Add an item (message / function call / function output) |
| `conversation.item.retrieve` | Fetch an item |
| `conversation.item.truncate` | Truncate a prior assistant audio item |
| `conversation.item.delete` | Remove an item from conversation history |
| `response.create` | Trigger a model response |
| `response.cancel` | Cancel an in-progress response |

#### 3.2 Server→client events (46 total)

Enumerated from the `RealtimeServerEvent` union
([`openapi.yaml#L51870-L51928`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L51870-L51928)).
Grouped for readability; the `type` strings are the wire contract:

**Session / conversation (4):** `session.created`, `session.updated`,
`conversation.created`, `conversation.item.retrieved`

**Conversation items (5):** `conversation.item.added`, `conversation.item.done`,
`conversation.item.created`, `conversation.item.deleted`,
`conversation.item.truncated`

**Input transcription (4):** `conversation.item.input_audio_transcription.delta`,
`...completed`, `...segment`, `...failed`

**Input audio buffer (6):** `input_audio_buffer.speech_started`,
`input_audio_buffer.speech_stopped`, `input_audio_buffer.committed`,
`input_audio_buffer.cleared`, `input_audio_buffer.timeout_triggered`,
`input_audio_buffer.dtmf_event_received`

**Output audio buffer (3):** `output_audio_buffer.started`,
`output_audio_buffer.stopped`, `output_audio_buffer.cleared`

**Response lifecycle (6):** `response.created`, `response.done`,
`response.output_item.added`, `response.output_item.done`,
`response.content_part.added`, `response.content_part.done`

**Response content deltas (6):** `response.output_text.delta`,
`response.output_text.done`, `response.output_audio.delta`,
`response.output_audio.done`, `response.output_audio_transcript.delta`,
`response.output_audio_transcript.done`

**Function calling (2):** `response.function_call_arguments.delta`,
`response.function_call_arguments.done`

**MCP (8):** `mcp_list_tools.in_progress`, `mcp_list_tools.completed`,
`mcp_list_tools.failed`, `response.mcp_call_arguments.delta`,
`response.mcp_call_arguments.done`, `response.mcp_call.in_progress`,
`response.mcp_call.completed`, `response.mcp_call.failed`

**Governance (2):** `rate_limits.updated`, `error`

⚠️ **GA renamed the response content events.** Migration guidance names the new
forms explicitly: `response.output_text.delta`, `response.output_audio.delta`,
`response.output_audio_transcript.delta` [S1]. Note the schema *names* still carry
the old shape (`RealtimeServerEventResponseAudioDelta` ⇒ wire
`response.output_audio.delta`), so a generator keyed on schema names will emit the
wrong wire strings. **Key on the `type` enum, not the schema name.**

### 4. Session lifecycle and audio frame encoding

**Lifecycle.** After the socket opens, the server sends `session.created`
"indicating the session is ready"; `conversation.created` is "emitted right after
session creation" [S3]
([`openapi.yaml#L51929-L51933`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L51929-L51933)).
The client may then send `session.update` at any time; the server responds with
`session.updated` "showing the full, **effective** configuration."

`session.update` merge semantics are explicit and asymmetric — this is a trap for
a proxy that normalizes payloads:

> Only the fields that are present in the `session.update` are updated. To clear a
> field like `instructions`, pass an empty string. To clear a field like `tools`,
> pass an empty array. To clear a field like `turn_detection`, pass `null`.

Immutability: every field may be updated **except `model`**, and `voice` "can be
updated only if there have been no other audio outputs yet" [S4]. Prose in the
conversations guide agrees: voice cannot change after the model has emitted audio [S3].

**Maximum session duration is 60 minutes** [S3].

`session.created` carries `session.type` = `realtime` (or `transcription`), plus
`object: "realtime.session"` and a `sess_...` id
([spec example](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L51870)).
Server-side default `instructions` are visible in that first event [S4].

**Audio is base64 inside JSON — never a binary WebSocket frame.** Both directions:

- **Input:** `input_audio_buffer.append` carries `audio`, typed `string`,
  "Base64-encoded audio bytes. This must be in the format specified by the
  `input_audio_format` field in the session configuration."
  ([`openapi.yaml` `RealtimeClientEventInputAudioBufferAppend`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L50019))
  Required fields are `type` and `audio`.
- **Output:** `response.output_audio.delta` carries base64 chunks. "the
  `response.output_audio.done` and `response.done` events won't actually contain
  audio data in them - just audio content transcriptions. To get the actual bytes,
  you'll need to listen for the `response.output_audio.delta` events" [S3].
- **Whole-message audio:** `conversation.item.create` with an `input_audio`
  content part whose `audio` is a base64 string [S3].

The guide states the design intent for the transport: over WebSocket "you will be
responsible for both sending and processing **Base64-encoded audio chunks** over
the socket connection" [S2], and "you will need to manually interact with the
input audio buffer by sending audio to the server, sent with JSON events with
base64-encoded audio" [S3].

**Chunk size limit — the two first-party sources disagree.** The spec says
"a maximum of **15 MiB**"
([`RealtimeClientEventInputAudioBufferAppend` description](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L50019));
the guide says "Each chunk cannot exceed **15 MB** in size" [S3]. Treat 15 MiB as
the ceiling only if the proxy must accept everything the spec permits; a
conservative frame limit is safer (§9.4).

**Audio formats** (`RealtimeAudioFormats`, GA): a tagged union of
`{"type":"audio/pcm","rate":24000}` (spec: "Only a 24kHz sample rate is
supported"), `{"type":"audio/pcmu"}` (G.711 μ-law), and `{"type":"audio/pcma"}`
(G.711 A-law). Configured at `session.audio.input.format` /
`session.audio.output.format` [S3].

**No ack for append.** "Unlike most other client events, the server will not send
a confirmation response to this event." A proxy must not wait on one.

### 5. Error event shape and close codes

**Error is an ordinary event on an open socket, not a connection teardown:**

> Returned when an error occurs, which could be a client problem or a server
> problem. **Most errors are recoverable and the session will stay open**, we
> recommend to implementors to monitor and log error messages by default.

([`RealtimeServerEventError`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L51887))

Envelope — required `event_id`, `type`, `error`; inside `error`, required `type`
and `message`, with nullable `code`, `param`, `event_id`:

```json
{
    "event_id": "event_890",
    "type": "error",
    "error": {
        "type": "invalid_request_error",
        "code": "invalid_event",
        "message": "The 'type' field is missing.",
        "param": null,
        "event_id": "event_567"
    }
}
```

The inner `error.event_id` is "The event_id of the client event that caused the
error, if applicable" — the correlation handle back to the offending client frame,
and the reason a proxy should preserve client-supplied `event_id` values verbatim.

**Close codes.** No first-party source publishes a table of Realtime-specific
WebSocket close codes. The only documented value is the **client-initiated normal
closure** default in both SDKs:

- Python: `async def close(self, *, code: int = 1000, reason: str = "")`
  ([`realtime.py#L368-L370`](https://github.com/openai/openai-python/blob/be928151372e4b62adb4a1571cda52ad759b38be/src/openai/resources/realtime/realtime.py#L368-L370))
- Node: "Closes the WebSocket with status code `1000` and reason `OK` by default."
  ([`ws.ts#L199-L210`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/realtime/ws.ts#L199-L212))

Server-side close codes are undocumented (§9.2).

### 6. Keepalive, ping/pong, and idle timeouts

**No application-level ping/pong event exists.** Neither union contains a
`ping`/`pong`/heartbeat event (§3.1, §3.2), and neither SDK configures WebSocket
ping intervals — the Python client passes only `user_agent_header` and
`additional_headers` plus caller-supplied `websocket_connection_options`
([`realtime.py#L713-L723`](https://github.com/openai/openai-python/blob/be928151372e4b62adb4a1571cda52ad759b38be/src/openai/resources/realtime/realtime.py#L713-L723)).
Keepalive is therefore left to RFC 6455 protocol-level ping/pong in the underlying
library (Python's `websockets` defaults), not to anything OpenAI specifies.

**The one documented timeout is a VAD turn timeout, not a connection idle
timeout.** `turn_detection.idle_timeout_ms` (server VAD only) is an integer
constrained to **5000–30000 ms**, default `null` (disabled):

> Optional timeout after which a model response will be triggered automatically.
> This is useful for situations in which a long pause from the user is unexpected,
> such as a phone call. … The timeout value will be applied after the last model
> response's audio has finished playing, i.e. it's set to the `response.done` time
> plus audio playback duration.

Reaching it emits `input_audio_buffer.timeout_triggered` plus a committed
`input_audio` item and a generated response — i.e. it **produces traffic rather
than closing the socket**
([`RealtimeTurnDetection`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L52871-L52884)).

The only hard connection bound documented anywhere is the **60-minute maximum
session duration** [S3]. Connection-level idle timeout is undocumented (§9.3).

### 7. Usage / token accounting

**Yes — reported in `response.done`, per Response, under `response.usage`.**
"The tokens used for a Response can be read from the `response.done` event" [S5].
Schema: `RealtimeResponse.usage`, described as "Usage statistics for the Response,
this will correspond to billing."

```json
{
  "type": "response.done",
  "response": {
    "usage": {
      "total_tokens": 253,
      "input_tokens": 132,
      "output_tokens": 121,
      "input_token_details": {
        "text_tokens": 119,
        "audio_tokens": 13,
        "image_tokens": 0,
        "cached_tokens": 64,
        "cached_tokens_details": { "text_tokens": 64, "audio_tokens": 0, "image_tokens": 0 }
      },
      "output_token_details": { "text_tokens": 30, "audio_tokens": 91 }
    }
  }
}
```

Field contract from the spec: `total_tokens`, `input_tokens`, `output_tokens` are
integers; `input_token_details` carries `cached_tokens`, `text_tokens`,
`image_tokens`, `audio_tokens` and a nested `cached_tokens_details`;
`output_token_details` carries `text_tokens` and `audio_tokens`. "Cached tokens
here are counted as a subset of input tokens."

**A second, separately-billed usage report exists.** Input transcription uses a
different model and rate card, and its tokens arrive on
`conversation.item.input_audio_transcription.completed` — with a differently
shaped `usage` object carrying a `"type": "tokens"` tag [S5]:

```json
{
  "type": "conversation.item.input_audio_transcription.completed",
  "transcript": "Hi, can you hear me?",
  "usage": { "type": "tokens", "total_tokens": 26, "input_tokens": 17,
             "input_token_details": { "text_tokens": 0, "audio_tokens": 17 },
             "output_tokens": 9 }
}
```

A gateway that only reads `response.done` will under-count whenever input
transcription is enabled.

**Rate limits are a third, separate channel.** `rate_limits.updated` is "Emitted at
the beginning of a Response", carrying `[{name: "requests"|"tokens", limit,
remaining, reset_seconds}]`. Critically, the reservation is provisional: "When a
Response is created some tokens will be 'reserved' for the output tokens, the rate
limits shown here reflect that reservation, which is then adjusted accordingly once
the Response is completed"
([`RealtimeServerEventRateLimitsUpdated`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L51894)).

**Billing model:** conversational sessions bill per-Response on input/output
tokens; **translation and transcription sessions are billed by audio duration**
and "don't use the normal Response lifecycle." "There is no cost currently for
network bandwidth or connections." Audio tokens: 1 token per 100 ms (user) and
1 token per 50 ms (assistant) [S5].

### 8. The WebSocket endpoint is absent from the OpenAPI spec

Worth stating because it determines what can be code-generated. `servers[0].url`
is `https://api.openai.com/v1`, and the `paths` object contains only HTTP realtime
routes — `/realtime/calls`, `/realtime/calls/{call_id}/{accept,hangup,refer,reject}`,
`/realtime/client_secrets`, `/realtime/sessions`, `/realtime/transcription_sessions`,
`/realtime/translations/client_secrets`. **There is no `/realtime` WebSocket path
item.** The spec contributes the *event schemas*; the *connection contract* (URL,
query params, subprotocols, headers) exists only in the guides and SDK source.
A generated client cannot be derived from the spec alone.

Ephemeral client credentials for untrusted clients come from
`POST /v1/realtime/client_secrets` [S1].

## 9. Undocumented / at-risk list

Design must make these explicit decisions rather than assume behavior:

1. **Negotiated subprotocol value.** Docs show what the client *offers*
   (§2); no source states what the server *selects* in its response
   `Sec-WebSocket-Protocol` header. A proxy that must echo a subprotocol has no
   documented value to echo — verify by capture.
2. **Server-initiated close codes.** Only the client-side default `1000`/`OK` is
   documented (§5). What the server sends on auth failure, session expiry, the
   60-minute cap, or overload is unspecified — including whether an HTTP status is
   observable at handshake time instead.
3. **Connection-level idle timeout.** Only the VAD `idle_timeout_ms`
   (5000–30000 ms, and it *generates traffic*) and the 60-minute session cap are
   documented (§6). Whether a silent socket is reaped, and after how long, is not.
4. **Append chunk ceiling: 15 MiB (spec) vs 15 MB (guide)** — the two first-party
   sources disagree (§4).
5. **Handshake failure shape.** No source describes how a bad key or bad `model`
   surfaces: HTTP error on upgrade vs. accepted socket followed by an `error` event
   then close.
6. **Whether the server ever sends binary frames.** All documented traffic is JSON
   text with base64 audio (§4); the SDKs expose a `recv_bytes()`/`parse_event()`
   pair, but no source says the server emits binary WebSocket frames.
7. **Event ordering guarantees** beyond the "rough order" the conversations guide
   gives; delta events are explicitly noted as possibly concurrent [S3].

## 10. Implications for the omugw WebSocket transport

Facts are above; design inference is isolated here.

1. **The transport is frame-agnostic but not opaque.** Every unit is a JSON text
   frame keyed by `type` (§3). A `dashscopewire`-style envelope decoder can be
   written against the `type` discriminator, but the transport must not assume a
   request/response pairing: `input_audio_buffer.append` gets **no ack** (§4), and
   `error` does **not** end the session (§5).
2. **`session.update` merge semantics forbid payload normalization.** Absent field
   ≠ cleared field; clearing requires `""` / `[]` / `null` per field (§4). A proxy
   that re-serializes through a struct with omitempty semantics will silently
   change session state. This is exactly the "转换是有损的，损失必须显式" case —
   byte-preserving passthrough is the safe default for `openai.realtime →
   openai.compat`.
3. **`event_id` is the correlation handle.** Client `event_id` is optional and
   echoed back only inside `error.event_id` (§3, §5). If the gateway rewrites or
   injects `event_id`, client-side error correlation breaks.
4. **Usage accounting needs two taps, not one.** `response.done.response.usage`
   plus `conversation.item.input_audio_transcription.completed.usage` (§7).
   Reading only the former under-counts. Because usage arrives per-Response on a
   long-lived socket, a session may produce *many* usage records — the existing
   `canonical.Usage` single-shot model does not obviously fit, and a stream that
   dies mid-session must mark `FidelityUnavailable` rather than report a partial
   sum.
5. **First-byte-then-no-failover applies immediately.** `session.created` is sent
   right after connect (§4), so the "already wrote downstream" flag is set within
   the first server frame. Realtime failover is effectively connect-time only.
6. **Auth rewriting must handle both carriers.** Header `Authorization: Bearer`
   *and* the `openai-insecure-api-key.<KEY>` subprotocol token (§2). A gateway that
   only rewrites the header will leak or drop the browser-style credential. Note
   the subprotocol token is by OpenAI's own naming "insecure" and is intended for
   **ephemeral** keys.
7. **No `OpenAI-Beta` header on GA (§1)** — but inbound clients may still send it,
   and its presence is a usable signal that a client expects the beta event names.
   GA vs beta differ in wire event names (`response.audio.delta` →
   `response.output_audio.delta`) and in session shape (§3.2), so the two are
   **not** interchangeable on the wire.
8. **Keepalive is the gateway's problem.** No application-level ping exists (§6);
   the gateway must run protocol-level ping/pong on both legs and pick its own
   idle policy, since upstream's is undocumented (§9.3). Also note `WriteTimeout`
   on the Go `http.Server` remains forbidden — same reason as long SSE streams.
9. **The 60-minute session cap is a hard product constraint** (§4) that any
   session-affinity or reconnect design must surface to clients rather than hide.

## Sources

Guides (retrieved 2026-09-06; append `.md` to any page URL for the Markdown form):

- [S1] [Realtime and audio (overview)](https://developers.openai.com/api/docs/guides/realtime) — transport choice; **beta→GA migration: remove `OpenAI-Beta: realtime=v1`**; GA event renames; `POST /v1/realtime/client_secrets`; `/v1/realtime` vs `/v1/realtime/translations`; `OpenAI-Safety-Identifier` header
- [S2] [Realtime API with WebSocket](https://developers.openai.com/api/docs/guides/realtime-websocket) — `wss://api.openai.com/v1/realtime?model=gpt-realtime-2.1`; `Authorization: Bearer`; browser subprotocol array (`realtime`, `openai-insecure-api-key.*`, `openai-organization.*`, `openai-project.*`); "JSON-serialized events as strings of text"; base64 audio responsibility
- [S3] [Realtime conversations](https://developers.openai.com/api/docs/guides/realtime-conversations) — session lifecycle; **60-minute max session**; voice immutability after audio output; WebSocket audio event flow table; `input_audio_buffer.append` base64 + 15 MB chunk statement; `response.output_audio.delta` carries the bytes; audio format config paths
- [S4] [Realtime client events reference](https://developers.openai.com/api/reference/resources/realtime/client-events) — `session.update` merge/clear semantics; `model` and `voice` immutability; `event_id` `maxLength: 512` and "not included in `session.updated`"; `RealtimeSessionCreateRequest` fields
- [S5] [Managing costs](https://developers.openai.com/api/docs/guides/realtime-costs) — `response.done` usage JSON; `conversation.item.input_audio_transcription.completed` usage JSON with `"type":"tokens"`; duration-based billing for translation/transcription; no bandwidth/connection cost; audio token rates; truncation/`retention_ratio`
- [S6] [Webhooks and server-side controls](https://developers.openai.com/api/docs/guides/realtime-server-controls) — sideband `wss://api.openai.com/v1/realtime?call_id=rtc_xxxxx`; `Location` header → call id; SIP socket lives for the call
- [S7] [Voice activity detection](https://developers.openai.com/api/docs/guides/realtime-vad) — `server_vad` vs `semantic_vad`; `session.audio.input.turn_detection` config

Specification — `openai/openai-openapi` @ [`b61ced96`](https://github.com/openai/openai-openapi/tree/b61ced96515cb6e73794ff459e9e12ca57596c72):

- [`RealtimeClientEvent` union](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L50009-L50025) — 11 client events, `discriminator: type`
- [`RealtimeServerEvent` union](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L51870-L51928) — 46 server events, `discriminator: type`
- [`RealtimeClientEventConversationItemCreate`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L50026-L50092) — client `event_id` optional, `maxLength: 512`
- [`RealtimeServerEventConversationCreated`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L51929-L51964) — server `event_id` required; "emitted right after session creation"
- `RealtimeClientEventInputAudioBufferAppend` — `audio` base64 string, required; 15 MiB max; "the server will not send a confirmation response to this event"
- `RealtimeServerEventError` — error envelope + `invalid_request_error` example; "most errors are recoverable and the session will stay open"
- `RealtimeServerEventRateLimitsUpdated` — `requests`/`tokens`, `limit`/`remaining`/`reset_seconds`; reservation-then-adjust semantics
- `RealtimeResponse.usage` — `total_tokens` / `input_tokens` / `output_tokens` / `input_token_details` (incl. `cached_tokens_details`) / `output_token_details`
- `RealtimeAudioFormats` — `audio/pcm` (rate fixed `24000`), `audio/pcmu`, `audio/pcma`
- [`RealtimeTurnDetection.idle_timeout_ms`](https://github.com/openai/openai-openapi/blob/b61ced96515cb6e73794ff459e9e12ca57596c72/openapi.yaml#L52871-L52884) — integer 5000–30000, default null; emits `input_audio_buffer.timeout_triggered`
- `paths` — realtime HTTP routes only; **no `/realtime` WebSocket path item**; `servers[0].url = https://api.openai.com/v1`

Official SDK source:

- [`openai-node` `src/realtime/internal-base.ts` @ `d8f6be93`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/realtime/internal-base.ts#L252-L305) — GA URL builder: exactly one of `model` / `intent=transcription` / `call_id`; `wss:` enforced
- [`openai-node` `src/realtime/websocket.ts` @ `d8f6be93`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/realtime/websocket.ts#L204-L239) — GA subprotocols `['realtime', 'openai-insecure-api-key.<KEY>']`; `wss:` guard; strict `type` parsing
- [`openai-node` `src/beta/realtime/websocket.ts` @ `d8f6be93`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/beta/realtime/websocket.ts#L216-L220) — **legacy** subprotocols add `'openai-beta.realtime-v1'`
- [`openai-node` `src/beta/realtime/ws.ts` @ `d8f6be93`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/beta/realtime/ws.ts#L71-L82) — **legacy** header `'OpenAI-Beta': 'realtime=v1'`
- [`openai-node` `src/realtime/ws.ts` @ `d8f6be93`](https://github.com/openai/openai-node/blob/d8f6be931ce315d41fd0605b03a37cc0c990e441/src/realtime/ws.ts#L199-L212) — close defaults to code `1000`, reason `OK`
- [`openai-python` `src/openai/resources/realtime/realtime.py` @ `be928151`](https://github.com/openai/openai-python/blob/be928151372e4b62adb4a1571cda52ad759b38be/src/openai/resources/realtime/realtime.py#L683-L734) — GA connect: auth headers only (no `OpenAI-Beta`), `model`/`call_id` query params, `ws`→`wss` scheme swap + `/realtime` path
- [`openai-python` `src/openai/resources/realtime/realtime.py#L368-L380` @ `be928151`](https://github.com/openai/openai-python/blob/be928151372e4b62adb4a1571cda52ad759b38be/src/openai/resources/realtime/realtime.py#L368-L380) — `close(code=1000, reason="")`; `parse_event()` over `str | bytes`
- [`openai-python` `src/openai/lib/_websocket.py` @ `be928151`](https://github.com/openai/openai-python/blob/be928151372e4b62adb4a1571cda52ad759b38be/src/openai/lib/_websocket.py) — cross-origin WebSocket redirects rejected (`SecurityError`); no ping/keepalive configuration
