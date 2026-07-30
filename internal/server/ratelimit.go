package server

import (
	"net"
	"net/http"
	"net/netip"
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

// clientIP identifies the requester for rate limiting.
//
// When TrustedProxies is empty (default), X-Forwarded-For is honored the same
// way as before: the RIGHTMOST value wins, matching a Cloudflare-fronted
// single-proxy deploy. When TrustedProxies is set, XFF is only trusted if
// RemoteAddr falls in that set — otherwise the direct peer address is used.
func (s *Server) clientIP(r *http.Request) string {
	return clientIP(r, s.cfg.TrustedProxies)
}

func clientIP(r *http.Request, trusted []netip.Prefix) string {
	host := remoteHost(r.RemoteAddr)
	honorXFF := len(trusted) == 0 || ipInPrefixes(host, trusted)
	if honorXFF {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.LastIndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[i+1:])
			}
			return strings.TrimSpace(xff)
		}
	}
	return host
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func ipInPrefixes(host string, prefixes []netip.Prefix) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
