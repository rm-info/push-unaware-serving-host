// Minimal IndexedDB helper shared by the page and the service worker.
// Stores: "kv" (client config: token, endpoint...) and "reminders" (the
// reminder texts, which never leave the device).
const DB_NAME = 'push-demo';
const DB_VERSION = 1;

function openDb() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, DB_VERSION);
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains('kv')) db.createObjectStore('kv');
      if (!db.objectStoreNames.contains('reminders')) db.createObjectStore('reminders', { keyPath: 'id' });
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

async function tx(store, mode, fn) {
  const db = await openDb();
  return new Promise((resolve, reject) => {
    const t = db.transaction(store, mode);
    const req = fn(t.objectStore(store));
    t.oncomplete = () => { db.close(); resolve(req && req.result); };
    t.onerror = t.onabort = () => { db.close(); reject(t.error); };
  });
}

const idb = {
  get: (store, key) => tx(store, 'readonly', (s) => s.get(key)),
  getAll: (store) => tx(store, 'readonly', (s) => s.getAll()),
  put: (store, value, key) => tx(store, 'readwrite', (s) => (key === undefined ? s.put(value) : s.put(value, key))),
  del: (store, key) => tx(store, 'readwrite', (s) => s.delete(key)),
};

// Usable both as a classic script (service worker: importScripts) and as a global.
self.idb = idb;
