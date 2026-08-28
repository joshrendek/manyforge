package provider

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// BuildFunc constructs a provider client from a fully resolved sending profile.
type BuildFunc func(context.Context, Profile) (Deliverer, error)

type cacheEntry struct {
	deliverer Deliverer
	updatedAt time.Time
	expiresAt time.Time
}

// Cache stores at most one concurrency-safe provider client per profile ID.
// A changed UpdatedAt value or an expired TTL causes the client to be rebuilt.
type Cache struct {
	mu      sync.Mutex
	entries map[uuid.UUID]cacheEntry
	build   BuildFunc
	ttl     time.Duration
	now     func() time.Time
}

// NewCache returns a provider cache. Non-positive TTLs use five minutes.
func NewCache(build BuildFunc, ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Cache{entries: make(map[uuid.UUID]cacheEntry), build: build, ttl: ttl, now: time.Now}
}

// Resolve returns the cached client for the profile ID and version, building it
// when absent, expired, or invalidated by a changed UpdatedAt timestamp.
func (c *Cache) Resolve(ctx context.Context, profile Profile) (Deliverer, error) {
	now := c.now()
	c.mu.Lock()
	if entry, ok := c.entries[profile.ID]; ok && entry.updatedAt.Equal(profile.UpdatedAt) && now.Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry.deliverer, nil
	}
	c.mu.Unlock()

	// Building never holds the cache mutex: credential resolution is local today,
	// but callers should remain free to add validation without serializing profiles.
	deliverer, err := c.build(ctx, profile)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[profile.ID] = cacheEntry{deliverer: deliverer, updatedAt: profile.UpdatedAt, expiresAt: now.Add(c.ttl)}
	c.mu.Unlock()
	return deliverer, nil
}
