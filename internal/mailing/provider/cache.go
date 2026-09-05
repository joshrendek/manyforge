package provider

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

const providerCacheCapacity = 32

// BuildFunc constructs a provider client from a fully resolved sending profile.
type BuildFunc func(context.Context, Profile) (Deliverer, error)

type cacheEntry struct {
	deliverer Deliverer
	updatedAt time.Time
	expiresAt time.Time
	lastUsed  uint64
}

// Cache stores a fixed number of concurrency-safe provider clients. Entries
// expire by TTL, and profile mutations explicitly invalidate credential copies.
type Cache struct {
	mu         sync.Mutex
	entries    map[uuid.UUID]cacheEntry
	build      BuildFunc
	ttl        time.Duration
	now        func() time.Time
	generation uint64
	clock      uint64
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
	c.removeExpiredLocked(now)
	c.clock++
	if entry, ok := c.entries[profile.ID]; ok && entry.updatedAt.Equal(profile.UpdatedAt) {
		entry.lastUsed = c.clock
		c.entries[profile.ID] = entry
		c.mu.Unlock()
		return entry.deliverer, nil
	}
	generation := c.generation
	c.mu.Unlock()

	deliverer, err := c.build(ctx, profile)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return deliverer, nil
	}
	c.removeExpiredLocked(c.now())
	for len(c.entries) >= providerCacheCapacity {
		c.removeLeastRecentlyUsedLocked()
	}
	c.clock++
	c.entries[profile.ID] = cacheEntry{
		deliverer: deliverer, updatedAt: profile.UpdatedAt,
		expiresAt: now.Add(c.ttl), lastUsed: c.clock,
	}
	return deliverer, nil
}

// Invalidate removes the credential-bearing client for profileID. The global
// generation also prevents an in-flight build from repopulating after mutation.
func (c *Cache) Invalidate(profileID uuid.UUID) {
	c.mu.Lock()
	delete(c.entries, profileID)
	c.generation++
	c.mu.Unlock()
}

func (c *Cache) removeExpiredLocked(now time.Time) {
	for id, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, id)
		}
	}
}

func (c *Cache) removeLeastRecentlyUsedLocked() {
	var oldestID uuid.UUID
	var oldest uint64
	first := true
	for id, entry := range c.entries {
		if first || entry.lastUsed < oldest {
			oldestID, oldest, first = id, entry.lastUsed, false
		}
	}
	if !first {
		delete(c.entries, oldestID)
	}
}
