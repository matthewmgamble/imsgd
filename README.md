# imsgd

**iMessage for your local network.** imsgd is a small daemon that runs on a Mac signed in to Messages.
It gives apps on your LAN a documented HTTP API to:

- **read** chats, messages, attachments and contact names;
- **receive** new messages, delivery and read receipts, edits and reactions in real time, over WebSocket or
  signed webhooks;
- **send** text and attachments to existing chats, phone numbers or email addresses, with confirmation that
  each message actually went out.

It's a single Go binary with no Electron, no cloud services and no Firebase. imsgd learned a lot from
[BlueBubbles](https://github.com/BlueBubblesApp/bluebubbles-server), but it is a new, smaller design, and its
API is not BlueBubbles-compatible.

```sh
curl -H "Authorization: Bearer $KEY" "http://mac.local:8642/api/v1/chats?limit=5&include=last_message"

curl -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"to":"+15551234567","text":"Dinner is ready 🍝","client_ref":"a1b2c3"}' \
  http://mac.local:8642/api/v1/messages
```

## Requirements

- macOS 13 or later, tested on macOS 26.5. You need a user account signed in to Messages, and it must stay
  logged in.
- Go 1.25+ to build.
- A code-signing identity, so macOS permissions survive rebuilds. A self-signed one is fine:
  `scripts/create-signing-cert.sh` creates it.

## Quick start (about 5 minutes)

```sh
git clone … imsgd && cd imsgd
scripts/create-signing-cert.sh        # once: a stable signing identity
make install                          # build, sign, install to ~/.local/bin/imsgd

imsgd keys create my-app              # prints an API key; store it
imsgd install-agent                   # start now and at every login
```

Then give imsgd its macOS permissions:

1. **Full Disk Access.** In System Settings → Privacy & Security → Full Disk Access, click **+**, press
   ⌘⇧G, enter `~/.local/bin/imsgd`, and turn it on.
2. **Automation.** Run `imsgd doctor -automation` (or send your first message) and click **Allow** when
   macOS asks whether imsgd may control Messages.
3. **Check.** Run `imsgd doctor` and `imsgd status`. Every line should be ✓.

imsgd logs the LAN URLs it is reachable at. See [docs/operations.md](docs/operations.md) for details and
troubleshooting.

## Documentation

| | |
|---|---|
| [docs/api.md](docs/api.md) | API guide: auth, pagination, sync, real-time events, webhooks, sending, attachments |
| [api/openapi.yaml](api/openapi.yaml) | OpenAPI 3.0 specification, also served at `/api/v1/openapi.yaml` |
| [docs/operations.md](docs/operations.md) | Installing, permissions, configuration, TLS, logs, upgrading, troubleshooting |
| [examples/](examples/) | A WebSocket client (Go) and a webhook receiver (Python) |

## Development

```sh
make test                              # unit, contract (OpenAPI) and integration tests
make fuzz                              # fuzz the typedstream decoder
IMSGD_LOAD=1 go test ./internal/api -run TestLoad -v   # load test
```

Commands that need Full Disk Access or Automation must run under launchd, because macOS applies
permissions to the program launchd started, not to your terminal. Two scripts help:

- `scripts/launchd-run.sh <imsgd args>` runs one command, e.g. `scripts/launchd-run.sh doctor`.
- `scripts/dev-agent.sh start|stop|logs` runs a development server.

Tests never touch your real Messages data. They build synthetic `chat.db` files from the real macOS
schema (`internal/imessage/fixture`).

## What it can't do

Sending uses AppleScript, so these features are **not available**: reactions, edits, unsends, inline
replies, typing indicators, marking chats read, and creating or managing group chats. They need Apple's
private frameworks (and disabling System Integrity Protection). imsgd still **reports** all of them when
they happen on other devices.

## Security

imsgd can read every message and send as you. Treat API keys like passwords:

- Give each app its own key (`imsgd keys create`) so you can revoke it on its own (`imsgd keys revoke`).
- Keep imsgd on a trusted network. Enable TLS (`imsgd gen-cert`) if other people share the LAN.
- Bind it to a single interface with `listen:` if the Mac has several.

See [docs/operations.md#security](docs/operations.md#security).

## License

[Apache License 2.0](LICENSE). See [NOTICE](NOTICE) for attributions.
