package server

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// statusWriter captures the response status and byte count for access logs.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController and friends reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack preserves websocket/hijack support if a handler needs it.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withAccessLog logs method, path, status, duration, bytes and client IP.
// Static assets and health probes are sampled down to reduce noise.
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		if skipAccessLog(r.URL.Path, status) {
			return
		}
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", sw.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", s.clientIP(r),
		)
	})
}

func skipAccessLog(path string, status int) bool {
	if path == "/healthz" && status < 400 {
		return true
	}
	// Successful static GETs are frequent and uninteresting.
	if status < 400 && strings.HasPrefix(path, "/static/") {
		return true
	}
	return false
}

// cacheControl wraps a handler and sets Cache-Control on successful responses
// that did not already specify one.
func cacheControl(value string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &cacheWriter{ResponseWriter: w, value: value}
		next.ServeHTTP(cw, r)
	})
}

type cacheWriter struct {
	http.ResponseWriter
	value string
	wrote bool
}

func (w *cacheWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		if code >= 200 && code < 400 && w.Header().Get("Cache-Control") == "" {
			w.Header().Set("Cache-Control", w.value)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cacheWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *cacheWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
