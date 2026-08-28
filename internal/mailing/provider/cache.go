package provider

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

type BuildFunc func(context.Context, Profile) (Deliverer, error)

type cacheKey struct {
	id        uuid.UUID
	updatedAt time.Time
}

type cacheEntry struct {
	deliverer Deliverer
	expiresAt time.Time
}

type Cache struct {
	mu      sync.Mutex
	entries map[cacheKey]cacheEntry
	build   BuildFunc
	ttl     time.Duration
	now     func() time.Time
}

func NewCache(build BuildFunc, ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Cache{entries: make(map[cacheKey]cacheEntry), build: build, ttl: ttl, now: time.Now}
}

func (c *Cache) Resolve(ctx context.Context, profile Profile) (Deliverer, error) {
	key := cacheKey{id: profile.ID, updatedAt: profile.UpdatedAt}
	now := c.now()
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && now.Before(entry.expiresAt) {
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
	for old := range c.entries {
		if old.id == profile.ID && old != key {
			delete(c.entries, old)
		}
	}
	c.entries[key] = cacheEntry{deliverer: deliverer, expiresAt: now.Add(c.ttl)}
	c.mu.Unlock()
	return deliverer, nil
}
