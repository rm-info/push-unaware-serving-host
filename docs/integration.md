# Integrating PUSH into a PWA

Principle: the server only knows an **opaque ID** and a **schedule**. The reminder text stays in the device's IndexedDB; your service worker looks it up when the push arrives.

Is PUSH for you?
- ✅ Notifications at known times: reminders, alarms, recurring nudges, computed on the device.
- ❌ Notifications triggered by server-side events ("you have a new message"): PUSH does not do that.

## 0. Prerequisites

- PWA served over **HTTPS**, with a manifest (`"display": "standalone"`) and a service worker.
- **iOS / iPadOS 16.4+**: Web Push only exists for a PWA **added to the Home Screen** and opened from its icon. Detect it (`display-mode: standalone`) and guide the user (Share → "Add to Home Screen").

## 1. Get an API key

Ask the operator of the PUSH server you want to use:

- If they enabled public requests, open `https://<their-push-server>/request-key`, fill in your app name, a contact e-mail, the origins where your PWA is served (e.g. `https://my-app.org`) and a short description. You get a **status token** (`req_…`): save it.
- Come back to `/request-key/status` with the token. Once approved, the page shows your **API key once**. Copy it. It is never stored in clear on the server; if you lose it, ask the operator to rotate it.
- Scripted alternative: `POST /v1/key-requests` then `GET /v1/key-requests/status` (see [api.md](api.md#api-key-requests)).

Your key only works from the origins you declared (in browsers), within the quotas set by the operator (devices, reminders per device).

Running your own server instead? See [operations.md](operations.md).

## 2. The client

Copy [`web/demo/push-client.js`](../web/demo/push-client.js) (ES module, no dependency):

```js
import { PushClient } from './push-client.js';

const registration = await navigator.serviceWorker.register('/sw.js');
const push = new PushClient({
  baseUrl: 'https://push.example.org',
  appKey: 'app_…',
  registration,
  storage, // optional; localStorage by default. Use IndexedDB if your service worker must read the token.
});

// On every app start: make sure the server knows the current subscription.
const { reset } = await push.sync();
if (reset) await push.replaceReminders(allLocalReminders()); // the server had forgotten this device

// Enabling MUST happen in a click handler (user gesture).
button.onclick = async () => {
  const { reset } = await push.enable();
  if (reset) await push.replaceReminders(allLocalReminders());
};
```

## 3. Create / delete reminders

```js
const id = crypto.randomUUID();
await db.put('reminders', { id, text: 'Take 1 tablet', schedule }); // text stays local
await push.putReminder(id, schedule);                              // the server sees id + schedule only

// Every day at 08:00, device time, DST-safe:
const schedule = { type: 'daily', time: '08:00', tz: PushClient.timeZone() };

await push.deleteReminder(id);
```

Always write locally **before** calling the server: when the push arrives, the text must exist.

Full resync (e.g. at start-up, or after restoring data):

```js
await push.replaceReminders(localList.map(r => ({ id: r.id, schedule: r.schedule })));
```

## 4. The service worker

```js
self.addEventListener('push', (event) => {
  event.waitUntil((async () => {
    let data = {};
    try { data = event.data.json(); } catch (_) {}
    const reminder = data.id ? await getFromIndexedDB('reminders', data.id) : null;
    // iOS RULE: ALWAYS show a notification, even when the ID is unknown.
    await self.registration.showNotification(reminder?.text || 'Reminder', {
      tag: data.id,
      data: { id: data.id },
    });
  })().catch(() => self.registration.showNotification('Reminder')));
});
```

**Never** receive a push without showing a notification: Safari may revoke the subscription after a few "silent" pushes. The final `.catch` guarantees a notification even if IndexedDB fails.

`data.t` holds the scheduled time (Unix seconds) if you want to display "scheduled for 08:00" when a notification arrives late.

Optional (Chrome/Firefox; Safari never fires it): handle `pushsubscriptionchange` by calling `PUT /v1/subscription` with the new subscription — see [`web/demo/sw.js`](../web/demo/sw.js). On Safari, `push.sync()` at start-up covers it.

## 5. Lifecycle and pitfalls

| Situation | Effect | What to do |
|---|---|---|
| PWA removed from the Home Screen / permission revoked | The subscription dies; the server deletes it at the next send (410) | Nothing server-side. At next launch `sync()` returns `reset: true` → `replaceReminders()` |
| Local data wiped | Token lost | `sync()` registers the device again (new token), `reset: true` |
| `403 app_disabled` | The operator revoked your key | Contact the operator |
| PUSH server down | Occurrences later than its grace period are skipped | Tell users delivery is not guaranteed |
| Several doses a day | — | One reminder per time |
| One-shot reminder in the past | Refused (`422 no_occurrence`) by `PUT /v1/reminders/{id}`, skipped by `PUT /v1/reminders` | — |

## 6. What to tell your users

- Notifications go through Apple/Google/Mozilla servers, which **do not guarantee** delivery or exact timing.
- No content is sent to the server: only a random ID and a time.
- On iPhone, the app must stay on the Home Screen with notifications allowed.

## Try it

Operators can enable a demo PWA with `PUSH_DEMO=true`: open `https://<server>/demo/`, add it to the Home Screen, paste an API key (the app's allowed origins must include the server's own origin), enable notifications and schedule a reminder in one minute.
