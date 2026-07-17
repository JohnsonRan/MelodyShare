package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	rl := newRateLimiter(3, time.Hour)
	rl.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if !rl.allow("a") {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	if rl.allow("a") {
		t.Fatal("4th request within window should be blocked")
	}
	if !rl.allow("b") {
		t.Fatal("independent key should not be affected")
	}
	now = now.Add(time.Hour)
	if !rl.allow("a") {
		t.Fatal("new window should allow again")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("POST", "/p", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	if got := clientIP(r); got != "10.0.0.1" {
		t.Fatalf("clientIP = %q, want 10.0.0.1", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 172.16.0.1")
	if got := clientIP(r); got != "172.16.0.1" {
		t.Fatalf("clientIP with XFF = %q, want 172.16.0.1 (rightmost)", got)
	}
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := clientIP(r); got != "198.51.100.7" {
		t.Fatalf("clientIP with single XFF = %q, want 198.51.100.7", got)
	}
}
