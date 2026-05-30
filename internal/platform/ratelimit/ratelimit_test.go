package ratelimit

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func TestTokenBucket(t *testing.T) {
	tb := NewTokenBucket(1, 2) // 1 tok/s, burst 2
	now := time.Unix(0, 0)
	tb.now = func() time.Time { return now }

	if !tb.Allow("k") {
		t.Fatal("first request within burst should be allowed")
	}
	if !tb.Allow("k") {
		t.Fatal("second request within burst should be allowed")
	}
	if tb.Allow("k") {
		t.Fatal("third request should be denied (bucket empty)")
	}
	now = now.Add(time.Second) // refill 1 token
	if !tb.Allow("k") {
		t.Fatal("request after 1s refill should be allowed")
	}
	if !tb.Allow("other") {
		t.Fatal("distinct key has its own bucket")
	}
}

func mustCIDR(s string) *net.IPNet { _, n, _ := net.ParseCIDR(s); return n }

func TestClientIP(t *testing.T) {
	trusted := []*net.IPNet{mustCIDR("10.0.0.0/8")}

	// Direct peer is a trusted proxy → use right-most untrusted XFF entry.
	r1 := &http.Request{RemoteAddr: "10.1.1.1:5000", Header: http.Header{"X-Forwarded-For": {"1.2.3.4, 10.0.0.2"}}}
	if got := ClientIP(r1, trusted); got != "1.2.3.4" {
		t.Errorf("trusted proxy: want 1.2.3.4, got %s", got)
	}

	// Direct peer is NOT trusted → ignore XFF (anti-spoof), use peer.
	r2 := &http.Request{RemoteAddr: "8.8.8.8:5000", Header: http.Header{"X-Forwarded-For": {"1.2.3.4"}}}
	if got := ClientIP(r2, trusted); got != "8.8.8.8" {
		t.Errorf("untrusted peer: want 8.8.8.8, got %s", got)
	}
}
