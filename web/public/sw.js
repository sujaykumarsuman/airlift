// airlift scan page service worker: an offline-first app shell for <base>s/*.
// Navigations to <base>s/{sid} are network-first with the last good scan.html as
// the fallback; hashed assets and the icons are cache-first. The API is never
// touched. BASE is the path prefix the worker was registered under ("/" or
// "/airlift/"), so it works both rooted and behind a reverse proxy (ADR 0012).
const CACHE = "airlift-shell-v1";
const BASE = new URL("./", self.location).pathname;
const SHELL_KEY = BASE + "s/";

self.addEventListener("install", () => {
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      const keys = await caches.keys();
      await Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)));
      await self.clients.claim();
    })(),
  );
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  const p = url.pathname;
  if (p.startsWith(BASE + "api/")) return;
  if (req.mode === "navigate" && p.startsWith(BASE + "s/")) {
    event.respondWith(networkFirst(req, SHELL_KEY));
    return;
  }
  if (p.startsWith(BASE + "assets/") || p.startsWith(BASE + "icons/") || p === BASE + "manifest.webmanifest") {
    event.respondWith(cacheFirst(req));
  }
});

async function networkFirst(req, key) {
  const cache = await caches.open(CACHE);
  try {
    const resp = await fetch(req);
    if (resp.ok) await cache.put(key, resp.clone());
    return resp;
  } catch (err) {
    const cached = await cache.match(key);
    if (cached) return cached;
    throw err;
  }
}

async function cacheFirst(req) {
  const cache = await caches.open(CACHE);
  const cached = await cache.match(req);
  if (cached) return cached;
  const resp = await fetch(req);
  if (resp.ok) await cache.put(req, resp.clone());
  return resp;
}
