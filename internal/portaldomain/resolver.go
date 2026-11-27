package portaldomain

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// maxResolverEntries bounds the cache so hostile Host headers cannot grow it.
const maxResolverEntries = 4096

// Lookup maps a normalised hostname to the business that has verified it.
type Lookup func(ctx context.Context, host string) (businessID uuid.UUID, ok bool, err error)

type resolverEntry struct {
	business uuid.UUID
	ok       bool
	expires  time.Time
}

// HostResolver resolves verified custom hostnames to businesses, caching hits and
// misses briefly. Errors are never cached.
type HostResolver struct {
	ttl   time.Duration
	now   func() time.Time
	fetch Lookup

	mu      sync.Mutex
	entries map[string]resolverEntry
}

// NewHostResolver returns a resolver backed by the database.
func NewHostResolver(database *db.DB, ttl time.Duration) *HostResolver {
	return &HostResolver{
		ttl:     ttl,
		now:     time.Now,
		entries: map[string]resolverEntry{},
		fetch: func(ctx context.Context, host string) (uuid.UUID, bool, error) {
			var id uuid.UUID
			err := database.WithTx(ctx, func(tx pgx.Tx) error {
				var qerr error
				id, qerr = dbgen.New(tx).ResolvePortalDomain(ctx, host)
				return qerr
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return uuid.Nil, false, nil
			}
			if err != nil {
				return uuid.Nil, false, err
			}
			return id, true, nil
		},
	}
}

// Lookup returns the business owning a verified hostname.
func (r *HostResolver) Lookup(ctx context.Context, host string) (uuid.UUID, bool, error) {
	host = NormalizeHost(host)
	now := r.now()
	r.mu.Lock()
	e, cached := r.entries[host]
	r.mu.Unlock()
	if cached && now.Before(e.expires) {
		return e.business, e.ok, nil
	}
	id, ok, err := r.fetch(ctx, host)
	if err != nil {
		return uuid.Nil, false, err
	}
	r.mu.Lock()
	if _, exists := r.entries[host]; !exists && len(r.entries) >= maxResolverEntries {
		r.entries = map[string]resolverEntry{}
	}
	r.entries[host] = resolverEntry{business: id, ok: ok, expires: now.Add(r.ttl)}
	r.mu.Unlock()
	return id, ok, nil
}

// Forget drops any cached answer for host so the next lookup reads the database.
func (r *HostResolver) Forget(host string) {
	host = NormalizeHost(host)
	r.mu.Lock()
	delete(r.entries, host)
	r.mu.Unlock()
}
