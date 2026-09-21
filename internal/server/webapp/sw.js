// Caches only the app shell. API responses are never cached.
const SHELL = "erka-shell-v1";
const FILES = ["./", "index.html", "app.css", "app.js", "manifest.webmanifest", "icon.svg"];
self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(SHELL).then((cache) => cache.addAll(FILES)).then(() => self.skipWaiting()));
});
self.addEventListener("activate", (event) => {
  event.waitUntil(caches.keys().then((keys) => Promise.all(keys.filter((key) => key !== SHELL).map((key) => caches.delete(key)))).then(() => self.clients.claim()));
});
self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);
  if (event.request.method !== "GET" || !url.pathname.startsWith("/app/")) return;
  event.respondWith(fetch(event.request).catch(() => caches.match(event.request)));
});
