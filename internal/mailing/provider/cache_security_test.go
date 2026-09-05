package provider

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/notify"
)

type retainedCredentialDeliverer struct{ credential string }

func (*retainedCredentialDeliverer) Send(context.Context, notify.Mail) (SendResult, error) {
	return SendResult{}, nil
}

// MF-MAIL-DELIVERY-001 requires expired credential clients to be evicted and
// bounds live clients even when every lookup uses a different profile.
func TestMFMailDelivery001ProviderCacheIsBoundedAndExpiresCredentials(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	cache := NewCache(func(_ context.Context, profile Profile) (Deliverer, error) {
		return &retainedCredentialDeliverer{credential: profile.ResendAPIKey}, nil
	}, time.Minute)
	cache.now = func() time.Time { return now }

	const profileCount = 64
	for i := range profileCount {
		credential := fmt.Sprintf("re_retained_%d", i)
		_, err := cache.Resolve(context.Background(), Profile{
			ID: uuid.New(), UpdatedAt: now, ResendAPIKey: credential,
		})
		if err != nil {
			t.Fatalf("resolve profile %d: %v", i, err)
		}
	}
	if len(cache.entries) > 32 {
		t.Fatalf("live cache entries = %d, want at most 32", len(cache.entries))
	}

	now = now.Add(2 * time.Minute)
	_, err := cache.Resolve(context.Background(), Profile{ID: uuid.New(), UpdatedAt: now, ResendAPIKey: "re_current"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cache.entries) != 1 {
		t.Fatalf("cache entries after TTL sweep = %d, want 1", len(cache.entries))
	}
}

func TestProviderCacheInvalidateForcesCredentialRebuild(t *testing.T) {
	builds := 0
	cache := NewCache(func(_ context.Context, profile Profile) (Deliverer, error) {
		builds++
		return &retainedCredentialDeliverer{credential: profile.ResendAPIKey}, nil
	}, time.Minute)
	profile := Profile{ID: uuid.New(), UpdatedAt: time.Now(), ResendAPIKey: "re_old"}
	if _, err := cache.Resolve(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	cache.Invalidate(profile.ID)
	profile.ResendAPIKey = "re_new"
	if _, err := cache.Resolve(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Fatalf("provider builds = %d, want 2 after invalidation", builds)
	}
}
