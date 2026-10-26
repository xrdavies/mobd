package api

import (
	"bufio"
	"compress/gzip"
	"net"
	"net/http"
	"strings"
	"sync"
)

// gzipMin is the smallest JSON body worth compressing.
const gzipMin = 1024

var gzipWriters = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed); return w }}

// streamWriters compress event streams. A reply streams as its whole text so far on each update,
// so most of an event repeats the previous one; level 5 finds those repeats far better than
// BestSpeed (a 10 KB reply in 150 updates: about 55x smaller instead of 25x) for similar CPU.
var streamWriters = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(nil, 5); return w }}

// withGzip compresses JSON answers and event streams for clients that accept gzip: a
// conversation is mostly text and shrinks about four times, and a stream far more. Images, video
// and WebSocket upgrades pass through untouched.
func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		g := &gzipWriter{ResponseWriter: w}
		defer g.close()
		next.ServeHTTP(g, r)
	})
}

// gzipWriter decides at WriteHeader: JSON and event streams are compressed, anything else is
// written as it is.
type gzipWriter struct {
	http.ResponseWriter
	decided bool
	zw      *gzip.Writer
	pool    *sync.Pool // where zw goes back
	code    int
	buf     []byte // the start of a JSON body, until it is large enough to compress
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.decided {
		return
	}
	g.decided, g.code = true, code
	h := g.Header()
	if h.Get("Content-Encoding") != "" || code < 200 || code == http.StatusNoContent || code == http.StatusNotModified {
		g.ResponseWriter.WriteHeader(code)
		g.code = 0
		return
	}
	switch ct := h.Get("Content-Type"); {
	case strings.HasPrefix(ct, "text/event-stream"):
		// Compressed from the first byte; each Flush ends the events written so far, so nothing
		// waits for more data.
		g.start(&streamWriters)
	case strings.HasPrefix(ct, "application/json"):
		g.buf = []byte{}
	default:
		g.ResponseWriter.WriteHeader(code)
		g.code = 0
	}
}

// start sends the headers of a compressed body and opens its writer.
func (g *gzipWriter) start(pool *sync.Pool) {
	h := g.Header()
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length")
	g.ResponseWriter.WriteHeader(g.code)
	g.pool = pool
	g.zw = pool.Get().(*gzip.Writer)
	g.zw.Reset(g.ResponseWriter)
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if !g.decided {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(p))
		}
		g.WriteHeader(http.StatusOK)
	}
	switch {
	case g.zw != nil:
		return g.zw.Write(p)
	case g.buf == nil:
		return g.ResponseWriter.Write(p)
	}
	g.buf = append(g.buf, p...)
	if len(g.buf) < gzipMin {
		return len(p), nil
	}
	g.start(&gzipWriters)
	if _, err := g.zw.Write(g.buf); err != nil {
		return 0, err
	}
	g.buf = nil
	return len(p), nil
}

// close finishes a compressed body, or writes a small JSON body as it is.
func (g *gzipWriter) close() {
	switch {
	case g.zw != nil:
		_ = g.zw.Close()
		g.pool.Put(g.zw)
		g.zw = nil
	case g.buf != nil:
		g.ResponseWriter.WriteHeader(g.code)
		_, _ = g.ResponseWriter.Write(g.buf)
		g.buf = nil
	}
}

// Flush sends what is written so far: a compressed stream is sync-flushed, so the client can
// decode every event at once; a small JSON body still being buffered waits for close.
func (g *gzipWriter) Flush() {
	if g.zw != nil {
		_ = g.zw.Flush()
	} else if g.buf != nil {
		return
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(g.ResponseWriter).Hijack()
}

func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }
