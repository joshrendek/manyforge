package ratelimit

import (
	"net"
	"net/http"
	"strings"
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
	if got, trustedPeer := ClientIPAndTrustedPeer(r1, trusted); got != "1.2.3.4" || !trustedPeer {
		t.Errorf("trusted proxy: want (1.2.3.4, true), got (%s, %t)", got, trustedPeer)
	}

	// Direct peer is NOT trusted → ignore XFF (anti-spoof), use peer.
	r2 := &http.Request{RemoteAddr: "8.8.8.8:5000", Header: http.Header{"X-Forwarded-For": {"1.2.3.4"}}}
	if got, trustedPeer := ClientIPAndTrustedPeer(r2, trusted); got != "8.8.8.8" || trustedPeer {
		t.Errorf("untrusted peer: want (8.8.8.8, false), got (%s, %t)", got, trustedPeer)
	}

	// The compatibility helper returns the same resolved address.
	if got := ClientIP(r1, trusted); got != "1.2.3.4" {
		t.Errorf("ClientIP: want 1.2.3.4, got %s", got)
	}
}

func TestClientIPBoundsForwardedFor(t *testing.T) {
	trusted := []*net.IPNet{mustCIDR("10.0.0.0/8")}
	tests := []struct {
		name string
		xff  string
		want string
	}{
		{
			name: "oversized header falls back to peer",
			xff:  strings.Repeat("1", maxForwardedForBytes+1),
			want: "10.1.1.1",
		},
		{
			name: "malformed hop falls back to peer",
			xff:  "1.2.3.4, not-an-ip",
			want: "10.1.1.1",
		},
		{
			name: "maximum depth resolves client",
			xff:  "1.2.3.4," + strings.Repeat("10.0.0.2,", maxForwardedForHops-2) + "10.0.0.2",
			want: "1.2.3.4",
		},
		{
			name: "excessive depth falls back to peer",
			xff:  "1.2.3.4," + strings.Repeat("10.0.0.2,", maxForwardedForHops-1) + "10.0.0.2",
			want: "10.1.1.1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{
				RemoteAddr: "10.1.1.1:5000",
				Header:     http.Header{"X-Forwarded-For": {tc.xff}},
			}
			got, trustedPeer := ClientIPAndTrustedPeer(r, trusted)
			if got != tc.want || !trustedPeer {
				t.Fatalf("ClientIPAndTrustedPeer() = (%q, %t), want (%q, true)", got, trustedPeer, tc.want)
			}
		})
	}
}

func TestIsTrustedPeer(t *testing.T) {
	trusted := []*net.IPNet{mustCIDR("10.0.0.0/8"), mustCIDR("2001:db8::/32")}
	for _, tc := range []struct {
		name       string
		remoteAddr string
		want       bool
	}{
		{name: "trusted IPv4 peer", remoteAddr: "10.1.2.3:5000", want: true},
		{name: "untrusted IPv4 peer", remoteAddr: "203.0.113.7:5000", want: false},
		{name: "trusted IPv6 peer", remoteAddr: "[2001:db8::7]:5000", want: true},
		{name: "malformed peer", remoteAddr: "not-an-ip", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tc.remoteAddr}
			if got := IsTrustedPeer(r, trusted); got != tc.want {
				t.Fatalf("IsTrustedPeer(%q) = %t, want %t", tc.remoteAddr, got, tc.want)
			}
		})
	}
}
