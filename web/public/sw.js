/* dworkspace service worker: app-shell caching only.
 *
 * Strategy:
 *  - Hashed /assets/* → cache-first (immutable by construction).
 *  - Navigations (index.html) → network-first, cache fallback, so the app
 *    shell opens offline but a deploy is picked up on the next online load.
 *  - EVERYTHING else (/api, /collab, /files, /mcp, /public) → network only.
 *    Caching API responses would serve stale user data; the CRDT and REST
 *    layers own their own consistency.
 */
const SHELL = 'dworkspace-shell-v1';

// How long a navigation waits for the network before the cached shell is used
// instead. Long enough that an ordinary slow load still comes from the network
// and nobody sees a stale shell for a hiccup — measured navigations answer in
// 0.15–0.5s — and short enough that a stall is not something you sit through.
const SHELL_TIMEOUT_MS = 3000;

// A sentinel rather than null or undefined: caches.match() resolves to
// undefined on a miss and fetch() can legitimately resolve to a falsy-looking
// value in no case that matters, but mixing the two makes the race below
// unreadable. This value can only have come from the timeout or a failure.
const SLOW = Symbol('slow');

self.addEventListener('install', (e) => {
  self.skipWaiting();
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== SHELL).map((k) => caches.delete(k))),
    ).then(() => self.clients.claim()),
  );
});

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== location.origin) return;

  // Immutable hashed assets: cache-first.
  if (url.pathname.startsWith('/assets/')) {
    e.respondWith(
      caches.open(SHELL).then(async (cache) => {
        const hit = await cache.match(e.request);
        if (hit) return hit;
        const res = await fetch(e.request);
        if (res.ok) cache.put(e.request, res.clone());
        return res;
      }),
    );
    return;
  }

  // App-shell navigations: network-first with cache fallback. Server-rendered
  // documents (share pages, ICS, API, uploaded files) are NOT the app shell —
  // caching their HTML under the '/' key would poison the offline shell, so we
  // only ever store a genuine React-app navigation there.
  if (e.request.mode === 'navigate') {
    const isServerDoc = /^\/(public|ics|api|files|collab|mcp)(\/|$)/.test(url.pathname);
    const network = fetch(e.request)
      .then((res) => {
        if (res.ok && !isServerDoc) {
          const copy = res.clone();
          caches.open(SHELL).then((c) => c.put('/', copy));
        }
        return res;
      });
    // A network that FAILS falls back to the cached shell; a network that is
    // merely slow used to fall back to nothing, because .catch() never fires
    // for a request that is still in flight. Opening a document was observed
    // taking 41 seconds on a stalled connection — the whole of it spent on this
    // one fetch, with a perfectly good shell sitting in the cache unused and
    // every API call beside it answering in under 200ms.
    //
    // So slow is treated as a kind of failure. The fetch is not aborted: it
    // goes on to refresh the cache, so the next load is current either way.
    //
    // Only app-shell navigations are raced. A share page or an ICS feed is a
    // different document that happens to be slow, and cutting it short to serve
    // the app shell instead would answer the wrong question — those keep the
    // behaviour they had, where only an outright failure falls back.
    e.respondWith(
      isServerDoc
        ? network.catch(() => caches.match('/'))
        : Promise.race([
            network.catch(() => SLOW),
            new Promise((resolve) => setTimeout(() => resolve(SLOW), SHELL_TIMEOUT_MS)),
          ]).then((res) => (res === SLOW ? caches.match('/').then((hit) => hit || network) : res)),
    );
  }
  // Everything else: default (network) — deliberately not cached.
});
