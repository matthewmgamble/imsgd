# Operating imsgd

## How it runs

imsgd runs as a **launchd agent** in the logged-in user's session (`~/Library/LaunchAgents/com.mgamble.imsgd.plist`).
It must run there rather than as a system daemon or from a terminal, for two reasons:

- **Messages runs per user.** imsgd sends by scripting the user's Messages app, and reads that user's
  `~/Library/Messages`.
- **macOS permissions apply to the program that launchd started.** A program started from Terminal, an IDE
  or another tool inherits *that app's* permissions instead. imsgd needs its own grants.

So the Mac must stay logged in to that account. The screen can be locked. Turn off automatic logout, and
consider setting the Mac to restart automatically after a power failure (System Settings → Energy).

| Command | |
|---|---|
| `imsgd install-agent [-config FILE]` | Writes the plist and starts imsgd. It restarts if it exits and starts at login. |
| `imsgd uninstall-agent` | Stops it and removes the plist. Config, data and logs are kept. |
| `imsgd status` | Agent state, a health check and the LAN URLs |
| `imsgd doctor [-automation]` | Checks signature, permissions, database access, contacts, config and TLS |
| `imsgd run` | Runs in the foreground (for debugging). Permissions only apply if launchd started it; see above. |

## Permissions

| Permission | Why | How to grant |
|---|---|---|
| **Full Disk Access** | Read `~/Library/Messages/chat.db`, attachments and Contacts | System Settings → Privacy & Security → Full Disk Access → **+** → ⌘⇧G → `~/.local/bin/imsgd` |
| **Automation → Messages** | Send messages | Approve the prompt that appears on the first send, or on `imsgd doctor -automation`. It's listed under Privacy & Security → Automation. |

**Code signing matters.** macOS ties these grants to the binary's code signature. Unsigned and ad-hoc-signed
builds get a new identity on every build, so the grants silently stop working after an upgrade.

- `scripts/create-signing-cert.sh` creates a self-signed "imsgd Local Code Signing" identity in your login
  keychain.
- `make install` signs with it.
- `imsgd doctor` warns if the binary isn't signed with a stable identity.

## Configuration

`~/.config/imsgd/config.yml` (see [`config.example.yml`](../config.example.yml)). Environment variables
`IMSGD_PORT`, `IMSGD_LISTEN`, `IMSGD_LOG_LEVEL`, `IMSGD_LOG_FILE`, `IMSGD_DATA_DIR` and `IMSGD_API_KEY`
override it.

| Key | Default | |
|---|---|---|
| `listen` | all interfaces | Bind address, e.g. `192.168.1.20` to listen on one network only |
| `port` | `8642` | |
| `api_keys` | none | Manage with `imsgd keys`. Changes apply to the running server within about 3 s. |
| `tls.cert_file`, `tls.key_file` | off | See [TLS](#tls) |
| `contacts.enabled`, `contacts.region` | `true`, Mac's region | Contact names; region is for numbers saved without a country code |
| `upload_max_mb` | `100` | Maximum size of a send request |
| `cache_max_mb` | `2048` | Converted-attachment cache |
| `cors_origins` | none | Browser origins allowed to call the REST API (`"*"` for any) |
| `send_rate_per_minute` | `60` | Sends per API key per minute (bursts of 20) |
| `log_level` | `info` | `debug`, `info`, `warn` or `error` |

Changes other than `api_keys` need a restart: run `imsgd install-agent` again, or
`launchctl kickstart -k gui/$(id -u)/com.mgamble.imsgd`.

### API keys

```sh
imsgd keys create home-assistant   # prints the key once
imsgd keys list                    # names only
imsgd keys revoke home-assistant
```

Every key can read all messages and send. Sends and webhooks are private to the key that created them.

### TLS

imsgd serves plain HTTP by default, which is fine on a home network you control. To encrypt traffic:

```sh
imsgd gen-cert                     # self-signed, valid for localhost, <host>.local and the Mac's LAN IPs
# add the printed tls: block to config.yml, then restart
```

Clients must trust the certificate. Either pin the SHA-256 fingerprint that `gen-cert` prints (most HTTP
libraries support pinning), or add `cert.pem` to the client's trust store. If the Mac's IP changes, re-run
`imsgd gen-cert -force`, or include stable names with `-hosts`. You can also use a certificate from your own
CA.

## Files

| Path | Contents |
|---|---|
| `~/.local/bin/imsgd` | The binary (`make install`) |
| `~/.config/imsgd/config.yml` | Configuration and API keys (mode 600) |
| `~/Library/Application Support/imsgd/imsgd.db` | Webhooks, delivery queue, send records |
| `~/Library/Application Support/imsgd/cache/` | Converted attachments, pruned to `cache_max_mb` |
| `~/Library/Messages/Attachments/imsgd/` | Files you sent through the API. **Don't delete them**: Messages references them from your conversation history. |
| `~/Library/Logs/imsgd/imsgd.log` | Log, rotated at 10 MB with 3 old copies kept |
| `~/Library/Logs/imsgd/launchd.log` | Startup failures and crashes only |

imsgd opens `chat.db` read-only and never modifies your Messages data.

## Upgrading

```sh
git pull && make install
launchctl kickstart -k gui/$(id -u)/com.mgamble.imsgd
imsgd status
```

The signature identity stays the same, so permissions carry over. Run `imsgd doctor` if something looks
wrong.

## Troubleshooting

| Symptom | Check |
|---|---|
| `imsgd status`: not responding | `tail ~/Library/Logs/imsgd/launchd.log`, the usual cause is an invalid config. Then `tail ~/Library/Logs/imsgd/imsgd.log`. |
| "cannot read …/chat.db" in the log | Full Disk Access isn't granted to `~/.local/bin/imsgd`, or the binary was rebuilt without the stable signature (`imsgd doctor`). |
| Sends fail with "Not authorized to send Apple events" | Automation permission. Re-enable imsgd → Messages under Privacy & Security → Automation. |
| Sends fail with "Messages reported error 22" | The address can't receive iMessage. Try `"service":"SMS"` (needs iPhone Text Message Forwarding). |
| A send stays `pending`, then fails with "did not appear" | Messages may be stuck. Quit and reopen it; imsgd relaunches it automatically if it's closed. |
| Attachments return `attachment_unavailable` | The file isn't on the Mac. Messages in iCloud may have offloaded it; open the conversation in Messages to download it. |
| Contact names are `null` | Names come from Contacts **on this Mac**. Enable your contacts account (e.g. iCloud) under System Settings → Internet Accounts. `imsgd doctor` shows how many contacts were found. |
| Port already in use | Another instance is running (`imsgd status`), or change `port:`. |

`imsgd doctor` checks most of these. For deeper investigation, set `log_level: debug`.

## Security

imsgd has the same access as the logged-in user to Messages and Contacts, and can send messages as them.

- **Network.** Bind to a specific interface with `listen:`. Don't expose the port to the internet or forward
  it through a router. If you need remote access, use a VPN (such as Tailscale or WireGuard) to reach the LAN.
- **Keys.**
  - Keys are compared in constant time and are only accepted in the `Authorization` header. The exception
    is `?token=` on the WebSocket endpoint, for browsers.
  - Query strings are never logged. Each request logs the *name* of the key that made it.
  - Create one key per app and revoke any you no longer use.
- **TLS.** Enable it on any network you don't fully trust. Without TLS, keys and messages cross the LAN in
  plain text.
- **Attachments.** Only files under `~/Library/Messages` are served, with symlinks resolved before the check.
  Uploads are stored with sanitized names.
- **Webhooks.**
  - Any key holder can register a webhook to any URL the Mac can reach, including other hosts on your LAN.
    imsgd treats every key as fully trusted, so only give keys to software you trust.
  - Verify the `X-Imsgd-Signature` header in every receiver.
- **Untrusted content.** Messages from strangers can contain anything. Attachments are served with
  `X-Content-Type-Options: nosniff`, and types that a browser could execute are served as downloads, not
  inline.
