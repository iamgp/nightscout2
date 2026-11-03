// Tideline service worker: caches the app shell so it opens instantly and
// offline. API data is never cached, so nothing stale is shown as current.
const CACHE = 'tideline-shell-v2';
const SHELL = ['./', 'index.html', 'app.js', 'style.css', 'icon.svg', 'manifest.webmanifest'];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== location.origin) return;
  if (url.pathname.startsWith('/api/') || url.pathname === '/pebble' || url.pathname === '/healthz') return;
  // Network first, so updates land immediately; fall back to cache offline.
  e.respondWith(
    fetch(e.request)
      .then((res) => {
        if (res.ok) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(e.request, copy));
        }
        return res;
      })
      .catch(() => caches.match(e.request).then((r) => r || caches.match('./'))),
  );
});

// Alerts pushed by the server.
self.addEventListener('push', (e) => {
  let msg = {};
  try { msg = e.data ? e.data.json() : {}; } catch { msg = { title: 'Tideline', body: e.data ? e.data.text() : '' }; }
  const urgent = msg.level === 'urgent' && !msg.resolved;
  e.waitUntil(self.registration.showNotification(msg.title || 'Tideline', {
    body: msg.body || '',
    tag: msg.tag || 'tideline',
    renotify: !msg.resolved,
    requireInteraction: urgent,
    silent: !!msg.resolved,
    icon: 'icon-192.png',
    badge: 'icon-192.png',
    vibrate: urgent ? [400, 200, 400, 200, 400] : [200, 100, 200],
    data: { url: msg.url || './#now' },
  }));
});

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const url = new URL((e.notification.data && e.notification.data.url) || './#now', self.registration.scope).href;
  e.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const w of wins) {
      if (w.url.startsWith(self.registration.scope)) { await w.focus(); return w.navigate(url); }
    }
    return self.clients.openWindow(url);
  })());
});
