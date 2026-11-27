package portaldomain

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

type resolverHarness struct {
	r     *HostResolver
	now   time.Time
	calls int
	biz   map[string]uuid.UUID
	err   error
}

func newResolverHarness() *resolverHarness {
	h := &resolverHarness{now: time.Unix(1_700_000_000, 0), biz: map[string]uuid.UUID{}}
	h.r = NewHostResolver(nil, 60*time.Second)
	h.r.now = func() time.Time { return h.now }
	h.r.fetch = func(_ context.Context, host string) (uuid.UUID, bool, error) {
		h.calls++
		if h.err != nil {
			return uuid.Nil, false, h.err
		}
		b, ok := h.biz[host]
		return b, ok, nil
	}
	return h
}

func TestResolverCachesHitAndMissForTTL(t *testing.T) {
	h := newResolverHarness()
	b := uuid.New()
	h.biz["hit.example.test"] = b
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		got, ok, err := h.r.Lookup(ctx, "hit.example.test")
		if err != nil || !ok || got != b {
			t.Fatalf("hit lookup = %v %v %v", got, ok, err)
		}
		if _, ok, _ := h.r.Lookup(ctx, "miss.example.test"); ok {
			t.Fatal("miss reported ok")
		}
	}
	if h.calls != 2 {
		t.Fatalf("calls within ttl = %d, want 2", h.calls)
	}
	h.now = h.now.Add(61 * time.Second)
	_, _, _ = h.r.Lookup(ctx, "hit.example.test")
	_, _, _ = h.r.Lookup(ctx, "miss.example.test")
	if h.calls != 4 {
		t.Fatalf("calls after ttl = %d, want 4", h.calls)
	}
}

func TestResolverForgetForcesRefetch(t *testing.T) {
	h := newResolverHarness()
	ctx := context.Background()
	_, _, _ = h.r.Lookup(ctx, "a.example.test")
	h.biz["a.example.test"] = uuid.New()
	h.r.Forget("A.Example.Test.")
	_, ok, _ := h.r.Lookup(ctx, "a.example.test")
	if !ok || h.calls != 2 {
		t.Fatalf("ok=%v calls=%d", ok, h.calls)
	}
}

func TestResolverBoundedCapacity(t *testing.T) {
	h := newResolverHarness()
	ctx := context.Background()
	for i := 0; i < 5000; i++ {
		_, _, _ = h.r.Lookup(ctx, fmt.Sprintf("h%d.example.test", i))
	}
	h.r.mu.Lock()
	n := len(h.r.entries)
	h.r.mu.Unlock()
	if n > 4096 || n == 0 {
		t.Fatalf("entries = %d", n)
	}
}

func TestResolverDoesNotCacheErrors(t *testing.T) {
	h := newResolverHarness()
	ctx := context.Background()
	h.err = errors.New("boom")
	if _, _, err := h.r.Lookup(ctx, "a.example.test"); err == nil {
		t.Fatal("want error")
	}
	h.err = nil
	if _, _, err := h.r.Lookup(ctx, "a.example.test"); err != nil {
		t.Fatalf("err cached: %v", err)
	}
	if h.calls != 2 {
		t.Fatalf("calls = %d", h.calls)
	}
}
