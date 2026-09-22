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

export default {
  async fetch(request) {
    const url = new URL(request.url);

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
