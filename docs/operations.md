# Running a PUSH server

## Configuration

Environment variables (or the equivalent flags, see `push serve -h`):

| Variable | Default | Purpose |
|---|---|---|
| `PUSH_VAPID_SUBJECT` | — (**required**) | VAPID contact, `mailto:…` or `https://…`. Must be real: Apple rejects placeholders. |
| `PUSH_DATA_DIR` | `./data` | SQLite database `push.db` + `vapid.json`. |
| `PUSH_LISTEN` | `127.0.0.1:8080` | Listen address (`0.0.0.0:8080` in the Docker image). |
| `PUSH_PUBLIC_URL` | derived from requests | Public base URL shown to integrators, e.g. `https://push.example.org`. |
| `PUSH_GRACE` | `1h` | Maximum lateness for an occurrence (downtime, retries). Beyond: **skipped**, not sent late. |
| `PUSH_TTL` | `1h` | How long push services keep a message for an offline device. |
| `PUSH_INTERVAL` | `15s` | Scan frequency. |
| `PUSH_ALLOW_ANY_PUSH_HOST` | `false` | By default only known push services (Google, Mozilla, Apple, Microsoft) are accepted as endpoints. Otherwise anyone holding an API key could make your server POST to arbitrary URLs, internal ones included (SSRF). |
| `PUSH_KEY_REQUESTS` | `false` | Enable the public key request form (`/request-key`) and its JSON API. |
| `PUSH_MAX_PENDING_REQUESTS` | `100` | Refuse new requests beyond this many pending ones. |
| `PUSH_ADMIN_UI` | `true` | Enable the web admin UI (`/admin/`). Inert until an admin account exists. |
| `PUSH_ADMIN_LISTEN` | — | Serve the admin UI **only** on this separate address (e.g. `127.0.0.1:8081`), not on the public listener. |
| `PUSH_ADMIN_SECURE_COOKIE` | `true` | Session cookie requires HTTPS (`localhost` works too). Set `false` only for plain HTTP on a trusted network. |
| `PUSH_TRUSTED_PROXIES` | `127.0.0.1/8,::1` | Reverse proxies whose `X-Forwarded-For` is trusted (rate limiting uses the client IP). Add your proxy's network if it is not on the same host, e.g. a Docker bridge `172.16.0.0/12`. |
| `PUSH_DEMO` | `false` | Serve the demo PWA under `/demo/`. |
| `PUSH_DEBUG` | `false` | Debug logs. |

## Public pages

| Path | Content |
|---|---|
| `/` | Landing page for integrators: what PUSH does, how to get a key on this server (links to `/request-key` when key requests are enabled), privacy summary. |
| `/llms.txt` | Single-file documentation for AI coding agents, generated from `docs/integration.md` + `docs/api.md` + the client source, with this server's URL and whether key requests are open. |
| `/push-client.js` | The reusable browser client. |

They use `PUSH_PUBLIC_URL` when set (otherwise the request's host).

## Applications and API keys

Each client PWA is an **application** with its own API key, allowed origins and quotas (max devices, max reminders per device). Three ways to create one:

1. **Admin UI** → *New application*: the key is shown once.
2. **CLI**: `push app add my-app -origins "https://my-app.org"`.
3. **Key request** (when `PUSH_KEY_REQUESTS=true`): the integrator fills `/request-key` and receives a status token. You review the request in the admin UI (or `push request list` / `approve` / `reject`). On approval the application is created **without a key**; the integrator retrieves it once with their status token. The key is generated at that moment and only stored hashed: you never see it.

New requests are logged (`new API key request awaiting review pending=N`, no personal data) and listed on the dashboard. No e-mail is sent: contact the requester at the address they gave if needed.

Revoking access:

| Action | Effect | Reversible |
|---|---|---|
| **Disable** | API calls refused (`403 app_disabled`), pushes paused, data kept | Yes (*Enable*). Occurrences missed meanwhile are skipped |
| **Rotate key** | New key shown to you once, old key refused immediately | — |
| **Delete** | App, key, devices and reminders erased | No |

Same actions in the CLI: `push app disable|enable|rotate-key|rm <name>`.

The API key of a PWA is necessarily readable in its code: treat it as an **app identifier**, not a secret. What protects you: allowed origins (enforced by browsers), quotas, the push-service allowlist, and rate limiting at your reverse proxy.

## Admin UI security

- Accounts are created with the CLI only: `push admin add <username>` (password ≥ 12 characters, bcrypt). `push admin passwd` changes a password and closes that account's sessions; `push admin rm` deletes an account.
- Sessions: random token in an `HttpOnly`, `Secure`, `SameSite=Strict` cookie (`__Host-` prefix), stored hashed, 12 h lifetime.
- Every state-changing form carries a per-session CSRF token; requests flagged cross-site by the browser (`Sec-Fetch-Site`) are refused.
- Failed logins are throttled: 10 per IP and 20 per username per 15 minutes. Unknown usernames take as long as wrong passwords.
- Pages contain no JavaScript and are served with a strict Content-Security-Policy, `X-Frame-Options: DENY`, `no-store`.
- Every operator action (UI or CLI) is recorded in an audit log shown on the dashboard (kept one year).

Recommended: don't expose `/admin/` to the Internet at all. Either set `PUSH_ADMIN_LISTEN=127.0.0.1:8081` and reach it through an SSH tunnel or VPN, or restrict `/admin/` at your reverse proxy (IP allowlist, client certificates, SSO).

## Reverse proxy

Web Push requires HTTPS. Examples in [`../deploy/`](../deploy/): nginx, Caddy and a hardened systemd unit. Recommendations:

- Disable access logs (they would contain reminder IDs in URLs).
- Rate limit `/request-key` and `/v1/subscriptions`.
- Preserve the `Host` header and send `X-Forwarded-Proto: https` and `X-Forwarded-For`.

## Backups

- `vapid.json`: **critical**. Back it up once, right after the first start. Losing it kills every subscription of every app; there is no recovery.
- `push.db`: back it up regularly with a consistent copy, e.g. `sqlite3 data/push.db ".backup '/backups/push.db'"` (safe while the server runs). Losing it loses applications and reminders, which PWAs can rebuild via `sync()` + `replaceReminders()`, but every app needs a new API key.

## Data retention

An hourly job deletes: expired admin sessions, rejected requests 30 days after the decision, approved requests 30 days after the key was claimed, audit entries older than a year. Pending requests and unclaimed approvals are kept until handled.

## Upgrades

The database schema is versioned and migrated automatically at start-up. A binary refuses to open a database created by a newer version. Back up `push.db` before upgrading.
