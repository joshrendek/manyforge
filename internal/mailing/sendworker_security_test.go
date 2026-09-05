package mailing

import (
	"testing"
	"time"

	"github.com/google/uuid"

	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	mailrender "github.com/manyforge/manyforge/internal/mailing/render"
)

// MF-MAIL-DELIVERY-001 characterizes the process-lifetime compiled-template
// cache. Every unique campaign/version key remains reachable with no eviction.
// After remediation, invert this test to assert the configured cache bound.
func TestMFMailDelivery001CompiledTemplateCacheHasNoBound(t *testing.T) {
	renderer, err := mailrender.New()
	if err != nil {
		t.Fatal(err)
	}
	worker := &SendWorker{Service: &Service{Renderer: renderer}}
	updatedAt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	profile := workerProfile{
		provider: mailprovider.Profile{ID: uuid.New(), UpdatedAt: updatedAt},
		fromName: "Audit sender",
	}

	const campaigns = 512
	for range campaigns {
		_, err = worker.compile(claimedDelivery{
			SourceID:         uuid.New(),
			ContentUpdatedAt: updatedAt,
			BodyMarkdown:     "# Unique cached campaign\n\nBody",
		}, profile)
		if err != nil {
			t.Fatal(err)
		}
	}

	entries := 0
	worker.compiled.Range(func(_, _ any) bool {
		entries++
		return true
	})
	if entries != campaigns {
		t.Fatalf("compiled cache entries = %d, want %d retained entries", entries, campaigns)
	}
}
