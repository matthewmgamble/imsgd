# imsgd API Guide

imsgd runs on a Mac signed in to Messages and lets applications on your local network read, receive
and send iMessages. This guide covers the concepts and common tasks. The exact schema for every endpoint
is in the OpenAPI document, [`api/openapi.yaml`](../api/openapi.yaml), which the server also serves at
`GET /api/v1/openapi.yaml`. Paste it into any OpenAPI viewer, or generate a client from it.

> Status: reading, real-time events (WebSocket and webhooks) and sending are implemented.

## Quick start

```sh
export IMSGD=http://192.168.1.20:8642          # the Mac's LAN address; imsgd logs it at startup
export IMSGD_KEY=...                           # one of the api_keys from ~/.config/imsgd/config.yml

curl -s $IMSGD/health
curl -s -H "Authorization: Bearer $IMSGD_KEY" "$IMSGD/api/v1/chats?limit=5&include=participants,last_message"
```

## Authentication

Every `/api/v1` request needs an API key in the `Authorization` header:

```
Authorization: Bearer <key>
```

Keys are listed in the config file (`~/.config/imsgd/config.yml`). Give each application its own named key,
so you can revoke one app by removing its key and restarting. A missing or wrong key returns `401`
`unauthorized`. `/health` needs no key.

## Conventions

| | |
|---|---|
| Field names | `snake_case` |
| Timestamps | RFC 3339, UTC: `2026-10-09T22:56:10Z` |
| Missing values | `null`. Fields are always present, except `participants` and `last_message` on chats, which only appear when requested. |
| IDs | Apple GUIDs, as opaque strings |
| Errors | `{"error": {"code": "not_found", "message": "chat not found"}}` |

### IDs and URL encoding

Chat IDs look like `any;-;+15551234567` (a direct chat on macOS 26) or `any;+;chat123456789` (a group).
Older macOS versions use `iMessage;-;…` or `SMS;-;…`. Don't parse them; just pass them back.

**Always percent-encode IDs** in URLs:
- In a **query string**, a raw `;` makes the whole query invalid, and imsgd returns `400`. A raw `+` would
  be read as a space.
- In a **path**, `;` and `+` work as is, but encoding them is still safest.

| Character | Encoded |
|---|---|
| `;` | `%3B` |
| `+` | `%2B` |

Every HTTP library has a helper for this:

```js
const chatId = "any;-;+15551234567";
fetch(`${base}/api/v1/chats/${encodeURIComponent(chatId)}/messages`, { headers });
```

```python
from urllib.parse import quote
requests.get(f"{base}/api/v1/messages", params={"chat_id": chat_id}, headers=headers)  # params are encoded for you
```

### Errors

| HTTP | `code` | When |
|---|---|---|
| 400 | `bad_request` | Invalid parameter, cursor or query string |
| 401 | `unauthorized` | Missing or invalid API key |
| 404 | `not_found` | Unknown chat, message, attachment or endpoint |
| 404 | `attachment_unavailable` | The attachment exists, but its file is not on this Mac |
| 405 | `method_not_allowed` | Wrong HTTP method |
| 413 | `payload_too_large` | The send request exceeds `upload_max_mb` |
| 422 | `conversion_failed` | An attachment couldn't be converted to the requested format |
| 429 | `rate_limited` | Too many sends, or too many open event streams, for this key. Honor `Retry-After`. |
| 500 | `internal` | A server error, which is logged on the Mac |

## Pagination

List endpoints (`/chats`, `/chats/{id}/messages`, `/handles`) return:

```json
{ "data": [ ... ], "next_cursor": "eyJrIjoiY2hhdHMiLC..." }
```

To get the next page, pass `next_cursor` back as `cursor`. It is `null` on the last page. `limit` sets the
page size: 1–1000, default 100. Cursors are opaque and tied to the listing that issued them. Using a chat
list cursor on a message list, or a `desc` cursor with `order=asc`, returns `400`.

```sh
cursor=""
while :; do
  page=$(curl -s -H "Authorization: Bearer $IMSGD_KEY" "$IMSGD/api/v1/chats?limit=200${cursor:+&cursor=$cursor}")
  echo "$page" | jq -r '.data[].id'
  cursor=$(echo "$page" | jq -r '.next_cursor // empty')
  [ -n "$cursor" ] || break
done
```

## Syncing messages

`GET /api/v1/messages?since_id=N` is built for keeping a local copy in sync. It returns messages whose
`rowid` is greater than `N`, oldest first, along with `next_since_id` and `has_more`. Row IDs only ever
increase, so storing `next_since_id` and calling again never skips a message.

```js
let sinceId = loadCheckpoint() ?? 0;          // 0 = full sync
for (;;) {
  const res = await api(`/api/v1/messages?since_id=${sinceId}&limit=1000`);
  for (const m of res.data) store(m);
  sinceId = res.next_since_id;
  saveCheckpoint(sinceId);
  if (!res.has_more) break;
}
```

A full sync of 33,000 messages takes about 4 seconds over the LAN. Add `chat_id=` to sync a single chat.

`since_id` returns **new** messages only. Changes to existing messages are a different matter: delivery
receipts, read receipts, edits and unsends. Those arrive as `message.updated` [events](#real-time-events).
Re-fetch a message with `GET /api/v1/messages/{id}` to see its latest state at any time.

## Real-time events

imsgd watches the Messages database and publishes an event within about a second of each change.
There are two ways to receive events:

- **WebSocket** at `GET /api/v1/events`: best for apps that stay connected.
- **Webhooks**: imsgd POSTs each event to your URL. Best for servers, and for when you can't hold a
  connection open.

Both deliver the same envelope:

```json
{
  "id": "9179b471-2",
  "type": "message.updated",
  "created_at": "2026-10-10T03:33:06Z",
  "data": { "message": { "id": "2F623AA1-…", "text": "on my way", "is_delivered": false, "…": "…" },
            "changes": ["sent"] }
}
```

| `type` | `data` | When |
|---|---|---|
| `message.received` | `{message}` | A new message from someone else |
| `message.sent` | `{message}` | A new message from you, sent from this Mac or any of your devices |
| `message.updated` | `{message, changes}` | An existing message changed. `changes` lists one or more of `sent`, `delivered`, `read`, `played`, `edited`, `unsent`, `failed`. `message` is the full current state. |
| `chat.read` | `{chat_id, read_at}` | You read a chat on one of your devices |
| `send.updated` | `{send}` | A send made through `POST /api/v1/messages` changed status. It is only delivered to the API key that made the send (on WebSocket and that key's webhooks). See [Sending](#sending). |

`message` is exactly the object returned by `GET /api/v1/messages/{id}`. Reactions (tapbacks) and group
changes arrive as `message.received`/`message.sent` with `message.type` set to `reaction` or
`group_event`.

A typical outgoing message produces three events: `message.sent`, then `message.updated` with
`changes: ["sent"]`, then `message.updated` with `["delivered"]`, followed later by `["read"]` if the
recipient shares read receipts. **If `changes` includes `failed`, check `message.error`.**

Notes:
- **Backfill is not announced.** When Messages downloads old history (for example from iCloud), those rows
  don't fire events. Only new rows dated within the last hour do. They are still returned by
  `GET /api/v1/messages?since_id=`.
- **Updates are tracked for 7 days.** Changes to messages older than that, such as a very late read
  receipt, don't produce events.

### WebSocket

```js
const ws = new WebSocket(`ws://mac.local:8642/api/v1/events?token=${KEY}&types=message.*`);
ws.onmessage = (e) => {
  const frame = JSON.parse(e.data);
  if (frame.type === "hello") return;           // first frame: {type:"hello", data:{resumed, ...}}
  lastEventId = frame.id;
  handle(frame);
};
```

Query parameters:

| Parameter | |
|---|---|
| `types` | Comma-separated event types to receive. `message.*` matches every message event. Default: all. |
| `chat_id` | Only events for this chat. Repeat it for several chats. |
| `last_event_id` | Resume after this event (see below) |
| `token` | The API key, for browsers. Other clients should send `Authorization: Bearer`. |

**Reconnecting without gaps.** Remember the `id` of the last event you handled, and reconnect with
`?last_event_id=<id>`. imsgd keeps the last 10,000 events and replays anything you missed before live events
resume. The first frame, `hello`, tells you whether that worked:

```json
{"type":"hello","data":{"server_version":"0.1.0","resumed":true,"last_event_id":"9179b471-3","replayed":2}}
```

If `resumed` is `false`, events were lost because the server restarted or you were away too long.
Catch up with `GET /api/v1/messages?since_id=<your checkpoint>`.

The server pings every 30 seconds. Close code `1001` means the server is shutting down. `1013` means your
client fell behind; reconnect with `last_event_id`.

`examples/watch` is a complete Go client that reconnects and resumes:

```sh
IMSGD_KEY=... go run ./examples/watch -url http://mac.local:8642 -types 'message.*'
```

> `?token=` puts the key in the URL, where proxies or browser history may record it. Prefer the
> `Authorization` header whenever the client can set it. imsgd never logs query strings.

Each key can hold up to 20 event streams open at once; more returns `429`.

### Webhooks

Register an endpoint:

```sh
curl -s -H "Authorization: Bearer $IMSGD_KEY" -H 'Content-Type: application/json' \
  -d '{"url":"http://192.168.1.50:3000/imsgd","events":["message.received"],"description":"home automation"}' \
  "$IMSGD/api/v1/webhooks"
```

| Field | |
|---|---|
| `url` | `http` or `https` |
| `events` | Type filters such as `message.*`. Empty means all. |
| `chat_ids` | Empty means all chats. |
| `enabled` | Default `true` |

**The response includes `secret` (`whsec_…`) once.** Store it; it's needed to verify deliveries.

Webhooks belong to the API key that created them. Other keys can't see, change or delete them, and they
stop delivering if that key is revoked. Changing a webhook's `url` generates a new secret, which is returned
in that `PATCH` response.

Each delivery is a `POST` with the event envelope as its JSON body and these headers:

| Header | |
|---|---|
| `X-Imsgd-Event` | Event type |
| `X-Imsgd-Event-Id` | Event ID, which is stable across retries. Use it to de-duplicate. |
| `X-Imsgd-Delivery` | Delivery ID |
| `X-Imsgd-Signature` | `t=<unix seconds>,v1=<hex HMAC-SHA256>` |

Delivery behaviour:
- Respond with any `2xx` within 10 seconds to acknowledge.
- Otherwise imsgd retries after 10 s, 30 s, 2 min, 10 min and 30 min, then marks the delivery `failed`.
- Deliveries are queued on disk, so they survive restarts.
- Ordering is best-effort, so use `created_at` when order matters.

| Endpoint | |
|---|---|
| `GET /api/v1/webhooks` | List |
| `POST /api/v1/webhooks` | Create |
| `GET /api/v1/webhooks/{id}` | Get one |
| `PATCH /api/v1/webhooks/{id}` | Update any of `url`, `events`, `chat_ids`, `description`, `enabled` |
| `DELETE /api/v1/webhooks/{id}` | Delete |
| `POST /api/v1/webhooks/{id}/test` | Sends a signed `webhook.test` event immediately and reports `{ok, status_code, error}` |
| `GET /api/v1/webhooks/{id}/deliveries` | Recent attempts with status and last error. Use it to debug a receiver. |

#### Verifying signatures

The signature is an HMAC-SHA256, keyed with the webhook secret, of `<t>.<raw body>`. Always verify against
the **raw** request body, before parsing it. Also reject old timestamps to block replays.

```python
import hashlib, hmac, time

def verify(secret: str, header: str, body: bytes, tolerance=300) -> bool:
    parts = dict(p.split("=", 1) for p in header.split(","))
    if abs(time.time() - int(parts["t"])) > tolerance:
        return False
    mac = hmac.new(secret.encode(), f"{parts['t']}.".encode() + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(mac, parts["v1"])
```

```js
import crypto from "node:crypto";

function verify(secret, header, rawBody, toleranceSec = 300) {
  const parts = Object.fromEntries(header.split(",").map((p) => p.split("=")));
  if (Math.abs(Date.now() / 1000 - Number(parts.t)) > toleranceSec) return false;
  const mac = crypto.createHmac("sha256", secret).update(`${parts.t}.`).update(rawBody).digest("hex");
  return crypto.timingSafeEqual(Buffer.from(mac), Buffer.from(parts.v1));
}
```

```go
// import "github.com/matthewmgamble/imsgd/internal/webhook" (within this module), or copy the function:
err := webhook.Verify(secret, r.Header.Get("X-Imsgd-Signature"), body, time.Now(), 5*time.Minute)
```

## Sending

```sh
# To an existing chat
curl -s -H "Authorization: Bearer $IMSGD_KEY" -H 'Content-Type: application/json' \
  -d '{"chat_id":"any;-;+15551234567","text":"On my way!","client_ref":"8c6f…"}' \
  "$IMSGD/api/v1/messages"

# To a phone number or email (Messages creates the conversation if needed)
curl -s -H "Authorization: Bearer $IMSGD_KEY" -H 'Content-Type: application/json' \
  -d '{"to":"(555) 123-4567","text":"Hello"}' "$IMSGD/api/v1/messages"

# With attachments: multipart, JSON fields in "payload", files in "attachments"
curl -s -H "Authorization: Bearer $IMSGD_KEY" \
  -F 'payload={"chat_id":"any;-;+15551234567","text":"Photos from today"}' \
  -F 'attachments=@IMG_0001.jpg' -F 'attachments=@IMG_0002.jpg' \
  "$IMSGD/api/v1/messages"
```

| Field | |
|---|---|
| `chat_id` *or* `to` | Exactly one. `to` accepts phone numbers in any common format, or an email address. |
| `service` | `iMessage` (default) or `SMS`, only with `to`. SMS needs Text Message Forwarding from an iPhone. |
| `text` | Up to 20,000 characters. Required unless you upload attachments. |
| `client_ref` | Your idempotency key (see below). Strongly recommended. |
| `wait` | Default `true`. Wait up to 60 s for confirmation. |

Each part is sent as its own message: the text first, then each attachment (at most 10, and 100 MB per
request by default, configurable with `upload_max_mb`).

### How confirmation works

Messages accepts a send immediately, but whether it worked only shows up in the Messages database a moment
later. Sending to an address that can't receive iMessage, for example, is "accepted" and then fails. So
imsgd tracks each part until it finds the message row it created, and reports the outcome in a **send**
object:

```json
{
  "send": {
    "id": "snd_adc72c1cb374e50d",
    "client_ref": "8c6f…",
    "status": "sent",
    "chat_id": "any;-;+15551234567",
    "to": null,
    "service": null,
    "error": null,
    "items": [
      { "index": 0, "kind": "text", "text": "On my way!", "filename": null,
        "status": "sent", "message_id": "31D4F29C-12F6-4238-A07F-B634D892EFCA", "error": null }
    ],
    "created_at": "2026-10-10T03:44:26Z",
    "updated_at": "2026-10-10T03:44:27Z"
  },
  "messages": [ { "id": "31D4F29C-…", "text": "On my way!", "is_sent": true, "…": "…" } ]
}
```

| `send.status` | Meaning |
|---|---|
| `pending` | Waiting for a part to appear in Messages, or to be sent |
| `sent` | Every part has been sent |
| `delivered` | Every part has been delivered |
| `failed` | At least one part failed. `error` explains why. |

Common failures:
- **The address can't receive iMessage.** Messages records error 22.
- **Messages rejected the send.** For example, the chat no longer exists.
- **The message never appeared.** This happens after 2 minutes for text and 20 minutes for attachments.

**Response codes:**

| Code | When |
|---|---|
| `201` | Every part was found in Messages (or failed). Check `send.status`, because `201` can still mean `failed`. |
| `202` | Still pending after 60 s, or `wait: false`. Follow up with `GET /api/v1/sends/{id}`, or listen for `send.updated` events, which carry the same `send` object. |
| `200` | An idempotent replay of an earlier send |
| `400` | Validation errors |
| `404` | Unknown `chat_id` |
| `413` | The upload is too large |

A typical text reaches `sent` in under a second and `delivered` a few seconds later.

### Idempotency and retries

Generate a unique `client_ref` (for example a UUID) for each message your app sends. If a request fails
or times out on the network, simply repeat it with the same `client_ref`. imsgd returns the original send,
with HTTP `200` and the header `Idempotent-Replay: true`, instead of sending a duplicate. `client_ref` is
scoped to your API key.

**imsgd never retries on its own.** When an AppleScript send times out, Messages may still have sent the
message, and retrying would send it twice. Decide in your app whether to retry a `failed` send, using a
**new** `client_ref`.

### Notes and limits

- **Sends are serialized.** Messages handles one send at a time, so a burst queues up.
- **Rate limit.** Each key may send 60 per minute, in bursts of up to 20 (configure with
  `send_rate_per_minute`). Beyond that you get `429 rate_limited` with `Retry-After`.
- **Matching is by content.** A part is matched to the first new outgoing message in the target chat with
  the same text (or attachment filename) after the send started. If you type the identical text on your
  phone into the same chat at the same moment, imsgd could pick that row instead. In practice this doesn't
  matter.
- **Uploaded files are kept.** They are stored under `~/Library/Messages/Attachments/imsgd/<month>/<send id>/`.
  Messages references these files from your conversation history, so imsgd never deletes them.
- **Group chats** can be messaged by `chat_id`. Creating new group chats, reactions, edits, unsends and
  replies are not supported: they need Apple's private APIs.
- **Messages must be running.** imsgd relaunches it in the background if it quits.
- **Restarts.** A send interrupted by an imsgd restart before its message was found is marked `failed`
  with an explanation. Check the chat before retrying.

## Reading chats

```sh
# Recent chats with participants and their latest message
curl -s -H "Authorization: Bearer $IMSGD_KEY" \
  "$IMSGD/api/v1/chats?limit=20&include=participants,last_message"

# Search by name, address or group name
curl -s -H "Authorization: Bearer $IMSGD_KEY" "$IMSGD/api/v1/chats?q=climbing"

# One chat (participants always included)
curl -s -H "Authorization: Bearer $IMSGD_KEY" "$IMSGD/api/v1/chats/any%3B-%3B%2B15551234567"
```

```json
{
  "id": "any;+;chat123456789",
  "display_name": "Climbing",
  "is_group": true,
  "identifier": "chat123456789",
  "service": "iMessage",
  "is_archived": false,
  "last_message_at": "2026-10-09T21:14:03Z",
  "last_read_at": "2026-10-09T21:15:40Z",
  "participants": [
    { "address": "+15551234567", "service": "iMessage", "name": null },
    { "address": "alex@example.com", "service": "iMessage", "name": null }
  ]
}
```

`participants` lists the *other* people in the chat; the account owner is not included. `name` is filled
in once contact resolution is enabled (a later sprint).

## Reading a chat's history

```sh
# Newest first (default); keep following next_cursor to scroll back
curl -s -H "Authorization: Bearer $IMSGD_KEY" \
  "$IMSGD/api/v1/chats/any%3B-%3B%2B15551234567/messages?limit=50"

# Oldest first
curl -s -H "Authorization: Bearer $IMSGD_KEY" \
  "$IMSGD/api/v1/chats/any%3B-%3B%2B15551234567/messages?order=asc"
```

## The message object

```json
{
  "id": "5003F179-45B0-4F2D-9EB7-7544EA7EA99E",
  "rowid": 339886,
  "chat_id": "any;-;+15551234567",
  "type": "message",
  "from_me": false,
  "sender": { "address": "+15551234567", "service": "iMessage", "name": null },
  "text": "look at this",
  "subject": null,
  "service": "iMessage",
  "parts": [
    { "index": 0, "text": "look at this", "attachment_id": null },
    { "index": 1, "text": "", "attachment_id": "E7A7ED59-24C8-4F1B-851F-A58A9728832C" }
  ],
  "mentions": [],
  "links": [],
  "attachments": [
    { "id": "E7A7ED59-24C8-4F1B-851F-A58A9728832C", "message_id": "5003F179-…", "filename": "IMG_0042.heic",
      "mime_type": "image/heic", "uti": "public.heic", "size": 1843210, "width": null, "height": null,
      "is_sticker": false, "is_outgoing": false, "available": true, "created_at": "2026-10-09T22:56:52Z" }
  ],
  "created_at": "2026-10-09T22:56:52Z",
  "delivered_at": null,
  "read_at": null,
  "edited_at": null,
  "is_sent": false,
  "is_delivered": true,
  "is_read": true,
  "error": 0,
  "unsent": false,
  "unsent_parts": [],
  "edits": [],
  "reaction": null,
  "group_event": null,
  "location_share": null,
  "reply_to_id": null,
  "effect": null,
  "app": null
}
```

Key fields:

- **`type`** says what kind of row this is:
  - `message`: ordinary text and/or attachments.
  - `reaction`: a tapback. See `reaction`, whose `kind` is love, like, dislike, laugh, emphasize,
    question, emoji or sticker. `target_id` is the message reacted to, and `removed: true` means the
    tapback was taken back.
  - `group_event`: someone was added, removed or left, or the group was renamed or its icon changed.
    See `group_event`.
  - `location_share`: a location-sharing notice.
- **`text`** is the plain text with attachment placeholders removed. It is empty for attachment-only
  messages, reactions and unsent messages.
- **`parts`** keeps the original layout when a message mixes text and attachments.
- **`from_me`** is true for messages sent by the account owner from *any* of their devices. `sender` is
  `null` for those.
- **Delivery state (your messages):**
  - `is_sent`, `is_delivered`, `delivered_at` and `read_at`. `read_at` only appears if the recipient shares
    read receipts.
  - **`error` ≠ 0 means Messages failed to send it.** For example, 22 means the recipient can't receive
    iMessage, and 25 means the attachment was rejected.
- **Edits and unsends:**
  - `edited_at` is set when a message was edited, and `edits` lists the earlier versions.
  - `unsent: true` means the whole message was unsent, so `text` is empty. `unsent_parts` lists the parts
    that were unsent individually.
- **`reply_to_id`** is the thread's original message, for inline replies.
- **`app`** is set for messages created by iMessage apps: link previews, Apple Pay, games and so on.

## Attachments

```sh
curl -s -H "Authorization: Bearer $IMSGD_KEY" "$IMSGD/api/v1/attachments/$ID"                    # metadata
curl -s -H "Authorization: Bearer $IMSGD_KEY" -o photo.heic "$IMSGD/api/v1/attachments/$ID/content"
curl -s -H "Authorization: Bearer $IMSGD_KEY" -o photo.jpg  "$IMSGD/api/v1/attachments/$ID/content?format=jpeg"
curl -s -H "Authorization: Bearer $IMSGD_KEY" -o thumb.jpg  "$IMSGD/api/v1/attachments/$ID/content?format=jpeg&max_size=320"
curl -s -H "Authorization: Bearer $IMSGD_KEY" -o live.mov   "$IMSGD/api/v1/attachments/$ID/content?format=live"
curl -s -H "Authorization: Bearer $IMSGD_KEY" -o voice.m4a  "$IMSGD/api/v1/attachments/$ID/content?format=m4a"
```

Metadata fields:

| Field | |
|---|---|
| `available` | The file is on this Mac. Many older attachments were never downloaded or were offloaded to iCloud, and their content returns `404 attachment_unavailable`. |
| `width` / `height` | For images, the **display** size: rotation is applied, so an iPhone portrait photo stored landscape reports e.g. `3024×4032`. `null` for other types and for unavailable files. |
| `live_photo` | The image has a Live Photo video, available with `format=live`. |
| `size` | The real file size when available, otherwise the size Messages recorded. |

`/content` options:

| `format` | Works for | Result |
|---|---|---|
| `original` (default) | Anything | The file as stored |
| `jpeg` | Images: HEIC, PNG, GIF (first frame), WebP, AVIF, TIFF and so on, up to 100 megapixels | A JPEG any browser can show. Add `max_size=N` (16–8192) to fit the longest edge in about N pixels, which is good for thumbnails. N is rounded up to the next of 64, 128, 256, 320, 480, 640, 800, 1024, 1280, 1600, 2048, 3072, 4096 or 8192, so thumbnails cache well. It never upscales. Rotation is preserved via EXIF orientation. |
| `m4a` | Audio, including Messages voice notes (CAF) | AAC audio in an M4A container |
| `live` | Images with `live_photo: true` | The Live Photo's QuickTime video |

Conversions are cached on the Mac, in `~/Library/Application Support/imsgd/cache` (up to `cache_max_mb`,
2 GB by default). A first conversion of a 12-megapixel HEIC takes about 0.25 s; repeats take a few
milliseconds. `Range` requests work for every format, so video players can seek. A file that fails to
convert returns `422 conversion_failed`.

Internal attachments, such as the data behind link previews, are hidden.

**Untrusted content.** Attachments come from whoever messaged you. Images, audio and video are served
`inline`. Everything else is served as a download (`Content-Disposition: attachment`). Types a browser could
execute, such as HTML, SVG, XML and JavaScript, are served as `application/octet-stream`. Every response
carries `X-Content-Type-Options: nosniff` and a sandboxing `Content-Security-Policy`.

## Contact names

When `contacts.enabled` is on (the default), imsgd reads the Mac's Contacts databases and fills in `name`
for handles, message senders and chat participants. The name is "First Last", else the nickname, else the
company.

- **Phone matching.** Numbers are normalized with libphonenumber, using your Mac's region, or
  `contacts.region` in the config, for numbers saved without a country code. As a fallback, they are
  matched on the last 10 digits.
- **Updates.** The index reloads automatically within a minute of a change in Contacts.
- **Where names come from.** Only contacts synced to **this Mac** are used. If names are missing, check
  that iCloud Contacts (or your contacts account) is enabled in System Settings → Internet Accounts on the
  Mac running imsgd.

## Handles

`GET /api/v1/handles?q=555` lists the phone numbers and emails Messages knows about, with cursor pagination.

## Server endpoints

| Endpoint | Purpose |
|---|---|
| `GET /health` | Liveness check. No auth. Returns `503` if the Messages database is unreadable. |
| `GET /api/v1/server` | Version, macOS version, start time, and `messages_db.max_rowid` (for sync progress) |
| `GET /api/v1/openapi.yaml` | The API specification |

## Running the server

See [operations.md](operations.md) for installing imsgd as a launchd agent, permissions, configuration,
TLS and troubleshooting.
