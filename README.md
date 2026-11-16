# PUSH — Push Unaware Serving Host

A small, **self-hosted, generic Web Push scheduler that knows nothing**. A Progressive Web App tells it "send a push to this device at this time, carrying this opaque ID", and PUSH does exactly that, without ever knowing the content, the purpose or the user.

- **One Go binary**, embedded SQLite (pure Go, no CGO): Linux, Windows, arm64, Docker.
- **Multi-application**: each PWA gets its own API key, quotas and allowed origins; data is isolated.
- **Zero content on the server**: a push only carries `{"id": "<opaque id>", "t": <scheduled time>}`. The service worker looks up the text locally (IndexedDB).
- **Time-zone aware recurrences**: "every day at 08:00, Europe/Paris" stays at 08:00 across daylight-saving changes.
- **API key requests**: third parties request a key through a public form; you approve or reject in a **web admin UI** (or the CLI); they retrieve the key themselves, once.
- Dead subscriptions (404/410) are cleaned up, transient failures retried with backoff, missed occurrences after downtime handled with a bounded grace period.

> ⚠️ Apple, Google and Mozilla **do not guarantee delivery** of push messages. If you use PUSH for critical reminders (medication…), tell your end users clearly.

## How it works

```
 PWA                                   PUSH                         Push service (Apple/Google/Mozilla)
  │ subscribe (VAPID public key)        │                                   │
  │ POST /v1/subscriptions ───────────► │ stores endpoint + keys            │
  │ PUT /v1/reminders/{uuid}  ────────► │ stores uuid + schedule            │
  │ (text stays in IndexedDB)           │                                   │
  │                                     │ at the scheduled time:            │
  │                                     │ encrypted push {"id": uuid} ────► │
  │ ◄──────────────────────────────────────────────────────────────────────  │
  │ service worker: uuid → local text → showNotification()                  │
```

## Quick start

```bash
make build                     # or: go build -o push ./cmd/push
./push admin add alice         # web admin account (asks for a password)
PUSH_VAPID_SUBJECT=mailto:you@your-real-domain ./push serve   # listens on 127.0.0.1:8080
```

Then open `http://localhost:8080/admin/`, create an application, and give its API key to the PWA. Or enable the public request form with `PUSH_KEY_REQUESTS=true` and let integrators apply at `/request-key`.

On the first `serve`, the VAPID key pair is generated in `data/vapid.json`.
**Back this file up.** Every push subscription is bound to the VAPID public key it was created with: if the file is lost or changed, **every subscription of every app dies**.

Docker:

```bash
mkdir data && sudo chown 65532:65532 data   # the image runs as uid 65532
# set PUSH_VAPID_SUBJECT in docker-compose.yml, then:
docker compose up -d
docker compose exec push push admin add alice
```

Web Push requires HTTPS: run PUSH behind your reverse proxy (examples in [`deploy/`](deploy/): nginx, Caddy, hardened systemd unit).

## Documentation

| Document | For |
|---|---|
| [`docs/integration.md`](docs/integration.md) | PWA developers: getting a key, client, service worker, iOS pitfalls |
| [`docs/api.md`](docs/api.md) | HTTP API reference |
| [`docs/operations.md`](docs/operations.md) | Operators: configuration, admin UI, key requests, security, backups |

The reusable browser client is [`web/demo/push-client.js`](web/demo/push-client.js); a complete demo PWA is served under `/demo/` with `PUSH_DEMO=true`.

## Privacy — what the server stores

| Data | Why |
|---|---|
| Apps: name, **hash** of the key, origins, quotas, maintainer contact | Authentication, isolation, abuse limits |
| Key requests: name, contact e-mail, origins, description | Operator review (purged 30 days after decision) |
| Subscriptions: endpoint + `p256dh`/`auth` keys, **hash** of the device token | Sending the encrypted push |
| Reminders: opaque ID, schedule, next occurrence | Knowing when to send what |
| Admin accounts (bcrypt), sessions (hashed), audit log of operator actions | Administration |

Nothing about end users: no reminder text, no account, no IP address, no access timestamps. Logs contain counters only (sent, skipped, failed…), never an endpoint or an ID.

## Development

```bash
make test    # go vet + go test ./...
make dist    # linux amd64/arm64 + windows amd64 binaries in dist/
```

License: [MIT](LICENSE).
