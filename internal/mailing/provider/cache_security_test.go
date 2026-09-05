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

// MF-MAIL-DELIVERY-001 characterizes the provider cache retaining every expired
// profile client, including its copied credential, for the process lifetime.
func TestMFMailDelivery001ProviderCacheRetainsExpiredCredentialClients(t *testing.T) {
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

	now = now.Add(2 * time.Minute)
	if len(cache.entries) != profileCount {
		t.Fatalf("expired cache entries = %d, want %d retained", len(cache.entries), profileCount)
	}
	for id, entry := range cache.entries {
		deliverer, ok := entry.deliverer.(*retainedCredentialDeliverer)
		if !ok || deliverer.credential == "" {
			t.Fatalf("profile %s no longer retains its credential-bearing client", id)
		}
	}
}
