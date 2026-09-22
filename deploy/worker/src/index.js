// A reverse proxy on *.workers.dev, so the instance is reachable under a second
// hostname when the first one is not.
//
// It is a proxy and nothing more: no caching, no rewriting of bodies, no
// inspection of traffic. What little it does add is the two headers the origin
// needs to know which hostname the BROWSER used, because dworkspace builds
// share links, invite links and OAuth redirect URIs from that — see
// server/users.go effectiveHost() and server/admin.go baseURL(). Without them
// every generated link would point back at the origin hostname, which is the
// hostname we are here to avoid.
//
// This hides nothing. The origin keeps its own public DNS record and its own
// public IP, and both still answer directly. If the goal were to hide the
// origin, this is the wrong tool — that needs the zone on Cloudflare with the
// record proxied.

const ORIGIN = "vuonuom.coolify.tuoitre.fun";

// Google will not switch an OAuth app to external production without a
// reachable home page and privacy policy. The app itself has neither: every
// path behind "/" is the workspace, and its front door is a login form, which
// describes nothing to somebody who is not a member yet.
//
// Serving them here rather than adding them to the app keeps two unrelated
// things apart. These pages exist for one console form; they are not a feature
// of the workspace, and they must stay reachable even when the origin is down —
// which is exactly when a reviewer is most likely to be looking.
//
// Both paths currently fall through to the SPA catch-all, so nothing is being
// shadowed. If the app ever grows a real /about or /privacy, these win and
// would have to move.
const PAGES = {
  "/about": {
    title: "About this workspace",
    body: `
      <p>This is a private, self-hosted instance of
      <strong>dworkspace</strong> — a workspace for notes, documents and
      structured pages, used by one team.</p>

      <p>It is not a public service and has no open registration for the
      general public. Accounts belong to people the operator of this instance
      has admitted.</p>

      <h2>Signing in</h2>
      <p>An account signs in with an email address and password, or with
      Google. Signing in with Google requests only the
      <code>openid</code>, <code>email</code> and <code>profile</code> scopes:
      enough to learn which account you are. It grants no access to Gmail,
      Drive, Calendar or contacts.</p>

      <p><a href="/privacy">Privacy policy</a></p>`,
  },
  "/privacy": {
    title: "Privacy policy",
    body: `
      <h2>What this instance stores</h2>
      <p>The content you create here — pages, documents, uploads, comments —
      is stored on a server operated privately by this instance's
      administrator. It is not sent to a third-party service.</p>

      <h2>What Google sign-in provides</h2>
      <p>When you choose to sign in with Google, Google returns your email
      address, your name and your profile picture. The email address is the
      only thing used to decide which account you are: it is matched against
      existing accounts on this instance.</p>

      <p>The requested scopes are <code>openid</code>, <code>email</code> and
      <code>profile</code> and nothing else. This instance cannot read your
      mail, your files, your calendar or your contacts, and does not ask to.
      No Google token is kept after sign-in completes; the session that follows
      is issued by this instance itself.</p>

      <h2>Who can see your content</h2>
      <p>Other members of a workspace you belong to, and the administrators of
      this instance, who by the nature of running the server can reach the
      underlying data.</p>

      <h2>Sharing</h2>
      <p>Content is not sold, and is not passed to advertisers or data
      brokers.</p>

      <h2>Removal</h2>
      <p>To have an account and its content removed, ask the administrator of
      this instance. Because the instance is self-hosted, that request is
      handled by them directly rather than by any third party.</p>

      <p><a href="/about">About this workspace</a></p>`,
  },
};

function page({ title, body }) {
  return new Response(
    `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>${title}</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0 auto; padding: 3rem 1.25rem 6rem; max-width: 42rem;
         font: 16px/1.65 system-ui, -apple-system, "Segoe UI", sans-serif; }
  h1 { font-size: 1.6rem; margin-bottom: 1.5rem; }
  h2 { font-size: 1.05rem; margin-top: 2rem; }
  code { font-size: 0.9em; }
  a { color: inherit; }
</style></head><body><h1>${title}</h1>${body}</body></html>`,
    {
      headers: {
        "content-type": "text/html; charset=utf-8",
        // Short, not immutable: these are edited by redeploying the Worker, and
        // a reviewer reloading after a correction should see the correction.
        "cache-control": "public, max-age=300",
      },
    },
  );
}

export default {
  async fetch(request) {
    const url = new URL(request.url);

    const known = PAGES[url.pathname];
    if (known && request.method === "GET") {
      return page(known);
    }

    // The hostname the browser actually typed. Captured before we rewrite the
    // URL, because that is the whole point of the exercise.
    const publicHost = url.host;

    url.protocol = "https:";
    url.hostname = ORIGIN;
    url.port = "";

    const headers = new Headers(request.headers);
    headers.set("X-Forwarded-Host", publicHost);
    headers.set("X-Forwarded-Proto", "https");
    // Host is not set here on purpose: fetch() derives both the Host header and
    // the TLS SNI from the URL, and Coolify's proxy routes on exactly those.

    const init = {
      method: request.method,
      headers,
      // Redirects belong to the browser. Following them here would resolve them
      // against the origin hostname and quietly walk the user off this proxy.
      redirect: "manual",
    };
    if (request.method !== "GET" && request.method !== "HEAD") {
      init.body = request.body;
    }

    const proxied = new Request(url, init);

    // WebSocket: the collaborative editor opens wss://<this host>/collab/<page>
    // (web/src/collab.ts), so an upgrade that is not carried through means
    // documents silently stop syncing while the rest of the app looks fine.
    // The origin answers 101 with a socket attached and we hand that same
    // socket back to the client; nothing here can read what flows over it.
    if (request.headers.get("Upgrade")?.toLowerCase() === "websocket") {
      const upstream = await fetch(proxied);
      if (upstream.webSocket) {
        return new Response(null, { status: 101, webSocket: upstream.webSocket });
      }
      // No socket means the origin declined, and it already said why: 401 for a
      // client that is not signed in, 403 for a disabled account, 404 for a page
      // that is gone. Answering 502 here would relabel every ordinary refusal as
      // a broken gateway and send people looking at the proxy instead of at
      // their session.
      return new Response(upstream.body, upstream);
    }

    let upstream;
    try {
      upstream = await fetch(proxied);
    } catch (err) {
      // Deliberately not falling through to passThroughOnException: the request
      // body has already been streamed upstream by now, so a retry would arrive
      // without it and fail as something misleading like a 400.
      return new Response(`origin unreachable: ${err.message}`, { status: 502 });
    }

    // dworkspace redirects with relative paths, so there is usually nothing to
    // fix. This covers the case where something absolute slips through — an
    // untouched Location would move the user back onto the origin hostname.
    const response = new Response(upstream.body, upstream);
    const location = response.headers.get("Location");
    if (location) {
      try {
        const target = new URL(location, url);
        if (target.host === ORIGIN) {
          target.host = publicHost;
          response.headers.set("Location", target.toString());
        }
      } catch {
        // Not a URL we can parse; leave it exactly as the origin wrote it.
      }
    }

    return response;
  },
};
