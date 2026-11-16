# PUSH HTTP API v1

All responses are JSON. Errors look like `{"error": "<code>", "message": "<text>"}`.

## Authentication

- `X-App-Key: app_…` — the application's API key. Required on every `/v1/` route except the VAPID key and key requests.
- `Authorization: Bearer sub_…` — the device's **management token**, returned by `POST /v1/subscriptions`. Required on reminder routes and `/v1/subscription`.

A device token is only valid together with the key of the app that issued it: apps are fully isolated.

Errors common to authenticated routes:

| Status | `error` | Meaning |
|---|---|---|
| 401 | `missing_app_key`, `invalid_app_key` | No / unknown API key |
| 403 | `app_disabled` | The operator revoked this app's access |
| 403 | `origin_not_allowed` | Browser request from an origin not allowed for this app |
| 401 | `missing_token`, `invalid_token` | No / unknown device token |

CORS: when an app has allowed origins, browser requests from other origins are refused. Without configured origins, any origin is accepted.

## Public routes

### `GET /healthz`
`200 {"status":"ok"}` or `503 {"status":"unavailable"}`.

### `GET /v1/vapid-public-key`
`200 {"publicKey":"B…"}` — pass it as `applicationServerKey` to `pushManager.subscribe()`.

## API key requests

Only available when the operator enabled them (`PUSH_KEY_REQUESTS=true`); `404` otherwise. The same flow exists as HTML pages at `/request-key`.

### `POST /v1/key-requests`

```json
{"name":"my-app","contact":"dev@my-app.org","origins":["https://my-app.org"],"description":"Medication reminders, ~500 users"}
```

- `name`: 2–63 characters, lowercase letters, digits and dashes.
- `contact`: e-mail address, used by the operator only.
- `origins`: at least one `https://` origin where the PWA is served.
- `description`: up to 1000 characters.

`202 {"status":"pending","status_token":"req_…"}`. **Keep the status token**: it is shown once and is the only way to retrieve the key.

Errors: `422 invalid_request`, `409 name_taken` (an app with this name exists), `429 rate_limited` (5 requests per hour and IP), `503 queue_full`.

### `GET /v1/key-requests/status`

Header `Authorization: Bearer req_…`.

```json
{"name":"my-app","status":"pending","created_at":"2026-11-16T10:00:00Z"}
{"name":"my-app","status":"rejected","reason":"…","created_at":"…"}
{"name":"my-app","status":"approved","app":"my-app","key_issued":true,"api_key":"app_…","created_at":"…"}
```

`api_key` is present **exactly once**: in the first response after approval. The key is generated at that moment and only its hash is stored; nobody, the operator included, can see it again. If it is lost, ask the operator to rotate it. `404 unknown_token`, `429 rate_limited`.

## Device (subscription)

### `POST /v1/subscriptions`
Body: the result of `PushSubscription.toJSON()`.

```json
{"endpoint":"https://web.push.apple.com/…","expirationTime":null,"keys":{"p256dh":"B…","auth":"…"}}
```

- `201 {"token":"sub_…","created":true}` — new device.
- `200 {"token":"sub_…","created":false}` — endpoint already known for this app: keys updated, **token renewed** (the old one stops working), reminders kept. Holding the subscription proves ownership of the device.
- `422 invalid_endpoint` (not https, or not a known push service), `422 invalid_keys`, `429 quota_exceeded`.

The token is stored hashed: keep it on the client.

### `PUT /v1/subscription`
Replaces endpoint and keys (e.g. after `pushsubscriptionchange`), keeping token and reminders. Same body. `204`, or `409 endpoint_exists` if that endpoint is registered under another token (register again with `POST /v1/subscriptions`).

### `DELETE /v1/subscription`
Forgets the device and all its reminders. `204`.

## Reminders

`{id}`: an opaque ID chosen by the client, 1–128 characters `[A-Za-z0-9_-]` (a UUID works). Unique per device.

### `PUT /v1/reminders/{id}`
```json
{"schedule": {"type":"daily","time":"08:00","tz":"Europe/Paris"}}
```
- `201` (created) or `200` (replaced): `{"id":"…","schedule":{…normalized…},"next_run_at":"2026-11-17T07:00:00Z"}`
- `422 invalid_id | invalid_schedule | no_occurrence` (e.g. `once` in the past), `429 quota_exceeded`.

### `DELETE /v1/reminders/{id}`
`204`, or `404 not_found`.

### `GET /v1/reminders`
`200 {"reminders":[{"id":"…","schedule":{…},"next_run_at":"…"}]}` — soonest first.

### `PUT /v1/reminders`
Atomically replaces the device's whole list (resync):
```json
{"reminders":[{"id":"a","schedule":{…}},{"id":"b","schedule":{…}}]}
```
Reminders without a future occurrence (e.g. past `once`) are silently skipped. Response: the resulting list (as `GET`). Errors: `422` with the `index` of the faulty item, `422 duplicate_id`, `429 quota_exceeded`.

## Schedules

| Type | Fields | Example |
|---|---|---|
| `once` | `at`: RFC 3339 with offset | `{"type":"once","at":"2026-11-17T08:00:00+01:00"}` |
| `daily` | `time` `HH:MM`, `tz` (IANA); optional `days` (`mon`…`sun`), `start`, `until` (`YYYY-MM-DD`, inclusive, local date) | `{"type":"daily","time":"08:00","tz":"Europe/Paris","days":["mon","fri"]}` |
| `interval` | `every` (1–366 days), `start`, `time`, `tz`; optional `until` | `{"type":"interval","every":2,"start":"2026-11-17","time":"21:00","tz":"Europe/Paris"}` |

Time zone: always the device's (`Intl.DateTimeFormat().resolvedOptions().timeZone`). Wall-clock time is preserved across DST changes. A time that does not exist (e.g. 02:30 on the spring-forward day) is moved to a valid time **on the same day**; an ambiguous time (fall-back day) fires once.

Several doses a day = several reminders (one per time).

## What the service worker receives

Payload (end-to-end encrypted, RFC 8291):

```json
{"id":"<opaque id>","t":1763362800}
```

`t` = scheduled time (Unix seconds, UTC), useful when a notification arrives late. Pushes are sent with `Urgency: high`, `TTL` = `PUSH_TTL`, and a `Topic` header derived from the ID: if the device was offline, only the latest pending push of a given reminder is delivered.

## Delivery behaviour

- Scan every `PUSH_INTERVAL` (15 s by default).
- `2xx`: `once` deleted, recurring advanced to the next occurrence.
- `404`/`410`: subscription dead → deleted with **all** its reminders.
- `429`/`5xx`/network error: retried (30 s, 1 min, 2 min… capped at 10 min, `Retry-After` honoured) while the delay stays ≤ `PUSH_GRACE`; otherwise the occurrence is dropped.
- Other `4xx`: occurrence dropped (retrying would not help).
- Server down: on restart, occurrences more than `PUSH_GRACE` late are **skipped** (no burst of stale reminders), others are sent.
- Disabled app: its pushes are paused; when re-enabled, the same grace rule applies.
