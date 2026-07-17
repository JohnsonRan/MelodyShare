package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateLimiter is a fixed-window per-key counter: at most limit events per key
// per window. State is in-memory only — good enough for abuse damping on a
// single-process server.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time // replaced in tests
	counts map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	n     int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, now: time.Now, counts: map[string]*rateWindow{}}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	w := rl.counts[key]
	if w == nil || now.Sub(w.start) >= rl.window {
		// starting a fresh window; occasionally sweep dead entries so the
		// map does not grow with one-off IPs forever
		if len(rl.counts) > 1024 {
			for k, v := range rl.counts {
				if now.Sub(v.start) >= rl.window {
					delete(rl.counts, k)
				}
			}
		}
		w = &rateWindow{start: now}
		rl.counts[key] = w
	}
	if w.n >= rl.limit {
		return false
	}
	w.n++
	return true
}

// clientIP identifies the requester for rate limiting. When X-Forwarded-For
// is present the RIGHTMOST value wins: it was appended by the nearest trusted
// proxy (Cloudflare), while earlier values are client-supplied and spoofable.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.LastIndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[i+1:])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
