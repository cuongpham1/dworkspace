package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve runs one request through the compression middleware wrapped around h.
func serve(h http.HandlerFunc, accept string, mut ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/x", nil)
	if accept != "" {
		r.Header.Set("Accept-Encoding", accept)
	}
	for _, f := range mut {
		f(r)
	}
	w := httptest.NewRecorder()
	compress(h).ServeHTTP(w, r)
	return w
}

func TestCompressShrinksJSONAndRoundTrips(t *testing.T) {
	body := `{"pages":[` + strings.Repeat(`{"title":"a page","snippet":"some text"},`, 500) + `{}]}`
	w := serve(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}, "gzip")

	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	// The point of the change: the bytes on the wire are a fraction of the
	// bytes the handler wrote.
	if w.Body.Len() >= len(body)/4 {
		t.Errorf("compressed to %d bytes from %d — expected at least 4x", w.Body.Len(), len(body))
	}
	gz, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Errorf("round trip changed the body")
	}
}

// A stale Content-Length describes the uncompressed body and would truncate
// the response at the client.
func TestCompressDropsContentLength(t *testing.T) {
	w := serve(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "13")
		io.WriteString(w, `{"ok":true}  `)
	}, "gzip")
	if got := w.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want it removed", got)
	}
}

func TestCompressSkipped(t *testing.T) {
	cases := []struct {
		name   string
		accept string
		ct     string
		mut    func(*http.Request)
	}{
		// Buffering SSE would hold every event until the buffer filled: the
		// stream stays open and delivers nothing.
		{name: "server-sent events", accept: "gzip", ct: "text/event-stream"},
		// Already compressed; a second pass only grows it.
		{name: "png", accept: "gzip", ct: "image/png"},
		{name: "woff2", accept: "gzip", ct: "font/woff2"},
		{name: "client did not ask", accept: "", ct: "application/json"},
		// "gzip;q=0" is how a client says it wants anything but gzip.
		{name: "gzip refused by q value", accept: "gzip;q=0", ct: "application/json"},
		// coder/websocket takes the connection over via http.Hijacker, which a
		// buffering wrapper cannot provide.
		{
			name: "websocket upgrade", accept: "gzip", ct: "application/json",
			mut: func(r *http.Request) { r.Header.Set("Upgrade", "websocket") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			muts := []func(*http.Request){}
			if tc.mut != nil {
				muts = append(muts, tc.mut)
			}
			w := serve(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				io.WriteString(w, strings.Repeat("x", 4096))
			}, tc.accept, muts...)
			if got := w.Header().Get("Content-Encoding"); got != "" {
				t.Errorf("Content-Encoding = %q, want none", got)
			}
			if w.Body.Len() != 4096 {
				t.Errorf("body = %d bytes, want 4096 untouched", w.Body.Len())
			}
		})
	}
}

// handleEvents asserts http.Flusher before it writes its first byte, and
// returns 500 when the assertion fails. The wrapper has to keep it satisfied
// whether or not this particular response ends up compressed.
func TestCompressKeepsFlusher(t *testing.T) {
	w := serve(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("handler lost http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: hello\n\n")
		fl.Flush()
	}, "gzip")
	if w.Body.String() != "data: hello\n\n" {
		t.Errorf("body = %q", w.Body.String())
	}
}

// A handler that encoded its own body owns it — /api/instance ships a gzipped
// tar, and a second wrapping would arrive as garbage.
func TestCompressLeavesPreEncodedBodyAlone(t *testing.T) {
	w := serve(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		io.WriteString(w, "already-compressed-bytes")
	}, "gzip")
	if got := w.Body.String(); got != "already-compressed-bytes" {
		t.Errorf("body = %q, want it untouched", got)
	}
}

// 206 offsets are counted against the uncompressed file and are already in
// Content-Range; compressing would make the response describe itself wrongly.
func TestCompressSkipsPartialContent(t *testing.T) {
	w := serve(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Range", "bytes 0-9/100")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "0123456789")
	}, "gzip")
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want none", got)
	}
	if w.Body.String() != "0123456789" {
		t.Errorf("body = %q", w.Body.String())
	}
}

// Caches key on it: without Vary, whichever variant was stored first is served
// to everybody, and half of them cannot read it.
func TestCompressAlwaysVaries(t *testing.T) {
	for _, accept := range []string{"gzip", ""} {
		w := serve(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, "{}")
		}, accept)
		if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("Accept-Encoding %q: Vary = %q", accept, w.Header().Get("Vary"))
		}
	}
}
