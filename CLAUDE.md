# PUSH — notes for Claude Code

Original design brief (French, historical): [`docs/brief.md`](docs/brief.md). This file summarizes the decisions and the rules to follow.

## Non-negotiable principle

The server **knows nothing**: it stores a push subscription + an opaque ID + a schedule, and sends `{"id","t"}`. Never any reminder text, end-user name, health data, or mention of a specific client app in code or API. Never log an endpoint or an ID (counters only). Operator-facing data (key requests, app contact e-mail, audit log) concerns integrators, not end users.

The project is open source: **code, comments, UI and docs in English**.

## Decisions

| Topic | Decision |
|---|---|
| Stack | **Go** 1.24 (keep `go 1.24.0` in go.mod: check that dependency upgrades don't raise it), single static binary, `modernc.org/sqlite` (pure Go, no CGO → trivial Windows/arm64 cross-compile), `SherClockHolmes/webpush-go` v1.4.0 (MIT), `golang.org/x/crypto/bcrypt`, `golang.org/x/term`. Standard `net/http` router, `html/template`. |
| Implementation | Written from scratch; webpush-go is the only third-party code to audit. |
| Schedules | JSON: `once` (`at` RFC 3339), `daily` (`time`, `tz`, `days?`, `start?`, `until?`), `interval` (`every` days, `start`, `time`, `tz`, `until?`). Evaluated in the device's IANA zone. |
| Quotas | Per app: max subscriptions (default 10,000) and max reminders per subscription (default 100). IP rate limiting of the API is left to the reverse proxy; key requests and admin logins have built-in limits. |
| App auth | API key (`X-App-Key`, stored hashed) + allowed origins per app (CORS). No request signing: a PWA's key is public anyway. Per device: management token (`Bearer`, hashed). |
| Missed occurrences | Sent if late ≤ `PUSH_GRACE` (1h), skipped otherwise. Same for retries. |
| Admin | Web UI at `/admin/` (server-rendered, no JS, strict CSP) + CLI. Accounts created via CLI only. |
| Key requests | Opt-in public form/API. Approval creates the app without a key; the requester claims it once with their status token (key generated at claim time, never stored in clear, no e-mail needed). |
| Revocation | Disable (reversible; API 403, pushes paused), rotate, delete. |
| License | MIT. |

Other choices:
- Endpoints restricted to known push services (FCM, Mozilla, Apple, WNS) by default → no SSRF. `PUSH_ALLOW_ANY_PUSH_HOST` lifts it.
- Re-registering an existing endpoint renews the token and keeps reminders (holding the subscription = owning the device).
- Payload `{"id","t"}`; headers `Urgency: high`, `TTL`, `Topic` = hash of the ID (collapse while offline).
- Scan every 15 s (`PUSH_INTERVAL`).
- webpush-go v1.4.0 prepends `mailto:` to any non-https subject → we strip ours first (otherwise `mailto:mailto:`, rejected by Apple). Tested in `pushsend_test.go`.
- Schema migrations are versioned in `internal/store/migrations.go`: never edit a released migration, append a new one, and add a test migrating from the previous version.

## Layout

```
cmd/push/            main: serve + admin CLI (app, request, admin, vapid)
internal/schedule/   schedule parsing and occurrences (time zones, DST)
internal/store/      SQLite: migrations, apps, subscriptions, reminders, key requests, admins, sessions, audit
internal/scheduler/  send loop, retries, 404/410 cleanup, grace
internal/pushsend/   Web Push sending (VAPID, RFC 8291), endpoint validation
internal/api/        HTTP API v1 + CORS (+ JSON key requests)
internal/keyreq/     key request logic shared by the HTML form and the JSON API
internal/portal/     public HTML pages /request-key
internal/admin/      web admin UI /admin/
internal/ui/         HTML templates + CSS (embedded)
internal/httpx/      client IP behind trusted proxies, rate limiter, security headers
internal/validate/   input rules shared by CLI, admin and portal
internal/vapid/      vapid.json generation/loading (0600, never overwritten)
web/demo/            demo PWA + reusable push-client.js (embedded)
deploy/              systemd, nginx, Caddy
docs/                api.md, integration.md, operations.md, brief.md
```

## Commands

```bash
go test ./...          # all tests (DST, app isolation, 410, retries, e2e encryption, migrations, admin flow)
make dist              # linux/windows binaries
PUSH_VAPID_SUBJECT=mailto:x@real-domain PUSH_KEY_REQUESTS=true go run ./cmd/push serve -demo
```

## To do / ideas

- Test the demo on a real iPhone (Home Screen) behind the reverse proxy.
- The Docker image is built and smoke-tested in CI only (no Docker daemon in dev sessions).
- Optional notification to operators when a key request arrives (webhook?).
- `push stats` / Prometheus metrics (counters only).
- Purge subscriptions without reminders for a long time (without storing access dates?).
