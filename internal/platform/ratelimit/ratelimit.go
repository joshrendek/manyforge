// Package ratelimit provides a token-bucket limiter and trusted-proxy client-IP
// resolution for the abuse-sensitive endpoints (auth, invite, hierarchy; FR-029).
// v1 is in-process; the Limiter interface allows a Postgres/Redis backend later.
package ratelimit

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limiter decides whether an action keyed by some identity is allowed now.
type Limiter interface {
	Allow(key string) bool
}

// TokenBucket is an in-process per-key token-bucket limiter.
type TokenBucket struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64 // max tokens
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewTokenBucket creates a limiter allowing burst immediate actions, refilling
// at rate tokens/second.
func NewTokenBucket(rate, burst float64) *TokenBucket {
	return &TokenBucket{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
		now:     time.Now,
	}
}

// Allow consumes a token for key, returning false if the bucket is empty.
func (t *TokenBucket) Allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	b, ok := t.buckets[key]
	if !ok {
		t.buckets[key] = &bucket{tokens: t.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * t.rate
	if b.tokens > t.burst {
		b.tokens = t.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ClientIP resolves the real client IP. The right-most untrusted address from
// X-Forwarded-For is used only when the direct peer is a trusted proxy;
// otherwise the direct peer address is authoritative (prevents header spoofing).
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if !isTrusted(net.ParseIP(peer), trusted) {
		return peer
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}
	parts := strings.Split(xff, ",")
	// walk right-to-left, skipping trusted proxies, to the first real client
	for i := len(parts) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(parts[i])
		if !isTrusted(net.ParseIP(ip), trusted) {
			return ip
		}
	}
	return peer
}

func isTrusted(ip net.IP, trusted []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, n := range trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
