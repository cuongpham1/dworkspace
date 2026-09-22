package server

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

// Response compression.
//
// Nothing here was compressed, and the page list is the reason it mattered: a
// workspace with a few thousand pages answers /api/pages with well over a
// megabyte of JSON that shrinks about eight-fold, and the browser asks for it
// three times while the app is starting. On a link that tops out around a
// megabyte a second that is the whole of the delay — the query itself returns
// in a third of a second and the server sits at zero percent CPU throughout.
// The same applies to the JS bundle, which is the largest single file the
// instance serves.
//
// It sits in ServeHTTP rather than around individual routes because the two
// biggest payloads come from opposite ends of the mux (an API handler and the
// embedded file server), and anything selective would have to be remembered
// every time a route is added.
//
// Compressing a response is not free of consequences for the two endpoints
// that do not return one:
//
//   - /collab/{id} upgrades to a WebSocket, and coder/websocket takes the
//     connection over through http.Hijacker. A wrapper that buffers bytes has
//     nothing to hijack, so upgrade requests are passed through untouched
//     rather than relying on the wrapper to forward Hijack correctly.
//   - /api/events is Server-Sent Events. It holds one response open for the
//     lifetime of the tab and flushes each event as it happens, so buffering
//     it would stall every event until the buffer filled — the stream would
//     look alive and deliver nothing. text/event-stream is excluded below, and
//     the wrapper implements Flusher so the handler's Flusher assertion still
//     succeeds on the way there.

// compressible answers whether a Content-Type is worth the CPU. Deliberately a
// list of what IS compressed rather than what is not: a type nobody considered
// should be sent as-is, and the types that dominate the bytes here are all
// known. JPEG, PNG, WebP and the fonts are already compressed and would only
// grow; text/event-stream is excluded for the streaming reason above.
func compressible(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	switch ct {
	case "application/json", "application/javascript", "text/javascript",
		"application/manifest+json", "application/xml", "image/svg+xml",
		"application/wasm":
		return true
	}
	// text/* except the event stream, which must not be buffered.
	return strings.HasPrefix(ct, "text/") && ct != "text/event-stream"
}

// gzipPool reuses the compressor's window. A gzip.Writer carries a few hundred
// kilobytes of state, and allocating one per response would hand the garbage
// collector more work than the compression saves.
var gzipPool = sync.Pool{
	New: func() any {
		// BestSpeed, not the default: on this JSON it is within a few percent
		// of the default ratio for a fraction of the CPU, and the point of the
		// exercise is the link, not the last byte.
		w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
		return w
	},
}

// gzipResponseWriter decides whether to compress at the moment the status is
// written, because that is the first point at which the handler's Content-Type
// is known. Until then it is an ordinary ResponseWriter.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz     *gzip.Writer
	wrote  bool // WriteHeader has run; gz is final
	status int
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wrote {
		return
	}
	g.wrote = true
	g.status = code

	h := g.ResponseWriter.Header()
	switch {
	// 204 and 304 carry no body at all, and 206 is a byte range counted
	// against the UNCOMPRESSED file — http.ServeContent has already put those
	// offsets in Content-Range, so compressing here would describe the
	// response incorrectly rather than merely inefficiently.
	case code == http.StatusNoContent, code == http.StatusNotModified,
		code == http.StatusPartialContent:
	// A handler that encoded its own body owns it: /api/instance ships a
	// gzipped tar and would arrive double-wrapped.
	case h.Get("Content-Encoding") != "":
	case !compressible(h.Get("Content-Type")):
	default:
		g.gz = gzipPool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
		h.Set("Content-Encoding", "gzip")
		// The length in the header counts the bytes the handler produced, and
		// fewer are about to go out. Leaving it would truncate the response.
		h.Del("Content-Length")
		// ETags are strong by default and identify the exact bytes; the
		// compressed body is not those bytes.
		if et := h.Get("ETag"); et != "" && !strings.HasPrefix(et, "W/") {
			h.Set("ETag", "W/"+et)
		}
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wrote {
		// net/http infers 200 from the first Write; do the same, so the
		// Content-Type sniffed or set by the handler is seen before deciding.
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Flush keeps streaming handlers streaming. handleEvents asserts http.Flusher
// before it writes anything, so this has to exist whether or not this
// particular response ends up compressed.
func (g *gzipResponseWriter) Flush() {
	if g.gz != nil {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// close finishes the gzip stream and returns the writer to the pool. Called
// from the middleware rather than the handler: a handler cannot know it was
// wrapped, and an unclosed stream loses the final block.
func (g *gzipResponseWriter) close() {
	if g.gz == nil {
		return
	}
	g.gz.Close()
	// Drop the reference to the response before pooling, or the writer keeps
	// the finished request's ResponseWriter alive until it is reused.
	g.gz.Reset(nil)
	gzipPool.Put(g.gz)
	g.gz = nil
}

// acceptsGzip reports whether the client asked for it. A bare substring match
// would also fire on "gzip;q=0", which is how a client says it wants anything
// BUT gzip.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		for _, p := range strings.Split(params, ";") {
			if q, ok := strings.CutPrefix(strings.TrimSpace(p), "q="); ok {
				if q == "0" || strings.HasPrefix(q, "0.0") {
					return false
				}
			}
		}
		return true
	}
	return false
}

// compress wraps a handler so responses go out gzipped when the client asked
// and the content type is worth it.
func compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Announced on every response, including the uncompressed ones: a
		// cache that saw the plain answer must not serve it to a client that
		// asked for gzip, or the other way round.
		w.Header().Add("Vary", "Accept-Encoding")

		// The WebSocket route needs the raw connection (see the note at the
		// top). Checking the request rather than trusting the wrapper to
		// forward Hijack keeps the failure mode out of reach entirely.
		if !acceptsGzip(r) || r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}

		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}
