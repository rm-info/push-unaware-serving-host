import { PushClient } from './push-client.js';

const $ = (id) => document.getElementById(id);
const log = (msg) => { $('log').textContent = `${new Date().toLocaleTimeString()}  ${msg}\n` + $('log').textContent; };

// Client config lives in IndexedDB so the service worker can read it too.
const storage = {
  get: async () => (await idb.get('kv', 'config')) || {},
  set: (v) => idb.put('kv', v, 'config'),
};

let push;

async function init() {
  const isiOS = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
  if (isiOS && !PushClient.isStandalone()) $('ios-hint').hidden = false;
  if (!PushClient.isSupported()) { setStatus('Web Push is not supported here.'); return; }

  const registration = await navigator.serviceWorker.register('sw.js', { scope: './' });
  await navigator.serviceWorker.ready;

  const cfg = await storage.get();
  $('app-key').value = cfg.appKey || '';
  push = new PushClient({ baseUrl: location.origin, appKey: cfg.appKey || '', registration, storage });

  if (cfg.appKey && Notification.permission === 'granted') {
    try { await afterSync(await push.sync()); } catch (e) { log(`sync: ${e.message}`); }
  }
  await render();
  setStatus(statusText());
}

function statusText() {
  if (Notification.permission === 'denied') return 'Notifications are blocked in the settings.';
  if (Notification.permission !== 'granted') return 'Notifications not enabled yet.';
  return 'Notifications enabled.';
}
function setStatus(s) { $('status').textContent = s; }

async function saveKey() {
  const appKey = $('app-key').value.trim();
  const cfg = await storage.get();
  if (cfg.appKey !== appKey) await storage.set({ ...cfg, appKey, baseUrl: location.origin });
  push.appKey = appKey;
  return appKey;
}

async function afterSync({ reset }) {
  if (!reset) return;
  // The server did not know this device: give it the full local list.
  const local = (await idb.getAll('reminders')).filter((r) => !r.fired);
  await push.replaceReminders(local.map((r) => ({ id: r.id, schedule: r.schedule })));
  log(`device (re)registered, ${local.length} reminder(s) re-sent`);
}

$('enable').onclick = async () => {
  try {
    if (!(await saveKey())) { log('enter the API key first'); return; }
    await afterSync(await push.enable()); // user gesture: permission prompt allowed
    setStatus(statusText());
    log('notifications enabled');
  } catch (e) { log(`enable: ${e.message}`); setStatus(statusText()); }
};

$('disable').onclick = async () => {
  try { await push.disable(); log('device forgotten by the server'); } catch (e) { log(`disable: ${e.message}`); }
};

$('mode').onchange = () => {
  $('in-wrap').hidden = $('mode').value !== 'in';
  $('daily-wrap').hidden = $('mode').value !== 'daily';
};

$('add').onclick = async () => {
  const id = crypto.randomUUID();
  const text = $('text').value.trim() || 'Reminder';
  const schedule = $('mode').value === 'in'
    ? { type: 'once', at: new Date(Date.now() + Number($('minutes').value) * 60000).toISOString().replace(/\.\d+Z$/, 'Z') }
    : { type: 'daily', time: $('time').value, tz: PushClient.timeZone() };
  try {
    await saveKey();
    await idb.put('reminders', { id, text, schedule }); // text stays here
    const res = await push.putReminder(id, schedule);   // server gets id + schedule only
    log(`scheduled: next push ${new Date(res.next_run_at).toLocaleString()}`);
  } catch (e) {
    await idb.del('reminders', id);
    log(`add: ${e.message}`);
  }
  await render();
};

$('resync').onclick = async () => {
  try {
    await saveKey();
    const local = (await idb.getAll('reminders')).filter((r) => !r.fired);
    await push.replaceReminders(local.map((r) => ({ id: r.id, schedule: r.schedule })));
    log('resynced');
  } catch (e) { log(`resync: ${e.message}`); }
  await render();
};

async function render() {
  const local = await idb.getAll('reminders');
  let remote = {};
  try { for (const r of await push.listReminders()) remote[r.id] = r; } catch (_) { /* not registered yet */ }
  const ul = $('list');
  ul.replaceChildren();
  if (!local.length) { ul.innerHTML = '<li class="muted">No reminders.</li>'; return; }
  for (const r of local) {
    const li = document.createElement('li');
    const s = remote[r.id];
    const when = s ? `next: ${new Date(s.next_run_at).toLocaleString()}` : (r.fired ? 'sent' : 'not on the server');
    const info = document.createElement('span');
    info.innerHTML = `<strong></strong><br><span class="muted"></span>`;
    info.querySelector('strong').textContent = r.text;
    info.querySelector('.muted').textContent = `${r.schedule.type === 'daily' ? `every day at ${r.schedule.time}` : 'once'} — ${when}`;
    const del = document.createElement('button');
    del.className = 'secondary';
    del.textContent = 'Delete';
    del.onclick = async () => {
      try { await push.deleteReminder(r.id); } catch (e) { log(`delete: ${e.message}`); }
      await idb.del('reminders', r.id);
      await render();
    };
    li.append(info, del);
    ul.append(li);
  }
}

init().catch((e) => log(`init: ${e.message}`));
