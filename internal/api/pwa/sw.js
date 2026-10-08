'use strict';
const CACHE = 'mirrin-shell-__VERSION__';
const HASHES = __HASHES__;
async function checked(response, hash) {
  if (!response || !response.ok) throw new Error('Missing shell');
  const digest = await crypto.subtle.digest('SHA-256', await response.clone().arrayBuffer());
  if (Array.from(new Uint8Array(digest), b => b.toString(16).padStart(2,'0')).join('') !== hash) throw new Error('Shell changed');
  return response;
}
self.addEventListener('install', event => event.waitUntil((async () => {
  const cache = await caches.open(CACHE);
  for (const [path, hash] of Object.entries(HASHES)) {
    const response = await checked(await fetch(path, {cache:'no-store', credentials:'omit'}), hash);
    await cache.put(path, response);
  }
  await self.skipWaiting();
})()));
self.addEventListener('activate', event => event.waitUntil((async () => {
  const cache = await caches.open(CACHE);
  for (const [path, hash] of Object.entries(HASHES)) await checked(await cache.match(path), hash);
  // Older shells go.
  for (const name of await caches.keys()) if (name.startsWith('mirrin-shell-') && name !== CACHE) await caches.delete(name);
  await self.clients.claim();
})()));
self.addEventListener('fetch', event => {
  const u = new URL(event.request.url);
  if (event.request.method !== 'GET' || u.origin !== self.location.origin || /^\/(events|screen|message|approvals|push|pair|stepup)(\/|$|\?)/.test(u.pathname) || /^\/(message|approvals|push|pair)/.test(u.pathname)) return;
  if (event.request.mode === 'navigate') {
    event.respondWith(fetch(event.request).catch(() => caches.open(CACHE).then(c => c.match('/offline'))));
  } else if (Object.hasOwn(HASHES, u.pathname) && !u.search) {
    event.respondWith(caches.open(CACHE).then(c => c.match(u.pathname)).then(r => r || fetch(event.request)));
  }
});
self.addEventListener('push', event => event.waitUntil((async () => {
  let p; try { p = event.data.json(); } catch { return; }
  if (p.v !== 1) return;
  try { if ('setAppBadge' in self.navigator) await self.navigator.setAppBadge(Math.max(0, p.badge || 0)); } catch { /* A badge failure must not hide the notification. */ }
  await self.registration.showNotification(p.t, {body:p.b, tag:p.tag, icon:'/icons/icon-192.png', data:{url:p.u}, silent:p.k === 'resolved', renotify:false});
  // A test notification tells the twin it arrived, so the computer's "Add your phone" page can say so.
  if (p.k === 'test') { try { await fetch('/push/received', {method:'POST', headers:{'Content-Type':'application/json'}, credentials:'same-origin', body:JSON.stringify({tag:'test'})}); } catch { /* the notification showed; only the tick on the computer is missed */ } }
})()));
self.addEventListener('notificationclick', event => {
  event.notification.close();
  const path = event.notification.data?.url;
  // A hand-over opens the screen with the page up; anything else, the screen.
  const target = /^\/approve\/\d+$/.test(path) || path === '/ui#browser' ? path : '/ui';
  event.waitUntil(self.clients.openWindow(target));
});
