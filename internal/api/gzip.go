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

// withGzip compresses JSON answers for clients that accept gzip: a conversation is mostly text
// and shrinks about four times. Event streams, images, video and WebSocket upgrades pass through
// untouched.
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

// gzipWriter decides at WriteHeader: JSON is compressed, anything else is written as it is.
type gzipWriter struct {
	http.ResponseWriter
	decided bool
	zw      *gzip.Writer
	code    int
	buf     []byte // the start of a JSON body, until it is large enough to compress
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.decided {
		return
	}
	g.decided, g.code = true, code
	h := g.Header()
	if !strings.HasPrefix(h.Get("Content-Type"), "application/json") || h.Get("Content-Encoding") != "" ||
		code < 200 || code == http.StatusNoContent || code == http.StatusNotModified {
		g.ResponseWriter.WriteHeader(code)
		g.code = 0
		return
	}
	g.buf = []byte{}
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
	h := g.Header()
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length")
	g.ResponseWriter.WriteHeader(g.code)
	g.zw = gzipWriters.Get().(*gzip.Writer)
	g.zw.Reset(g.ResponseWriter)
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
		gzipWriters.Put(g.zw)
		g.zw = nil
	case g.buf != nil:
		g.ResponseWriter.WriteHeader(g.code)
		_, _ = g.ResponseWriter.Write(g.buf)
		g.buf = nil
	}
}

// Flush sends what is written so far (event streams are never compressed).
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
