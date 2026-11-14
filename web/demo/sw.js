// Demo service worker. Rule #1 on iOS: EVERY push must show a visible
// notification, otherwise Safari may revoke the subscription.
importScripts('idb.js');

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => event.waitUntil(self.clients.claim()));

self.addEventListener('push', (event) => {
  event.waitUntil(handlePush(event).catch(() =>
    // Whatever happened, still show something.
    self.registration.showNotification('Rappel', { icon: 'icon-192.png' })
  ));
});

async function handlePush(event) {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch (_) { /* not JSON */ }
  const reminder = data.id ? await idb.get('reminders', data.id) : null;
  const title = (reminder && reminder.text) || 'Rappel'; // generic fallback if unknown locally
  const when = data.t ? new Date(data.t * 1000) : null;
  await self.registration.showNotification(title, {
    body: when ? `Prévu à ${when.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}` : '',
    tag: data.id || undefined,
    icon: 'icon-192.png',
    badge: 'icon-192.png',
    data: { id: data.id || null },
  });
  if (reminder && reminder.schedule && reminder.schedule.type === 'once') {
    reminder.fired = true;
    await idb.put('reminders', reminder);
  }
}

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  event.waitUntil((async () => {
    const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    if (all.length) return all[0].focus();
    return self.clients.openWindow('./');
  })());
});

// Chrome/Firefox may rotate the subscription; tell the server, keeping the
// same token and reminders. (Safari does not fire this event: the page
// re-checks the subscription on every launch instead.)
self.addEventListener('pushsubscriptionchange', (event) => {
  event.waitUntil((async () => {
    const cfg = await idb.get('kv', 'config');
    if (!cfg || !cfg.token) return;
    const sub = event.newSubscription || await self.registration.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: cfg.vapidKey,
    });
    await fetch(`${cfg.baseUrl}/v1/subscription`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', 'X-App-Key': cfg.appKey, Authorization: `Bearer ${cfg.token}` },
      body: JSON.stringify(sub.toJSON()),
    });
    await idb.put('kv', { ...cfg, endpoint: sub.endpoint }, 'config');
  })());
});
