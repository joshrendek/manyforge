package mailing

import (
	"testing"
	"time"

	"github.com/google/uuid"

	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	mailrender "github.com/manyforge/manyforge/internal/mailing/render"
)

// MF-MAIL-DELIVERY-001 requires the compiled-template cache to evict entries
// before tenant-authored content can grow the worker heap without bound.
func TestMFMailDelivery001CompiledTemplateCacheIsEntryBounded(t *testing.T) {
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

	entries, bytes := worker.compiled.stats()
	if entries > compiledCacheMaxEntries {
		t.Fatalf("compiled cache entries = %d, want at most %d", entries, compiledCacheMaxEntries)
	}
	if bytes > compiledCacheMaxBytes {
		t.Fatalf("compiled cache bytes = %d, want at most %d", bytes, compiledCacheMaxBytes)
	}
}

func TestCompiledContentCacheExpiresAndInvalidatesImmutableVersions(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	cache := newCompiledContentCache(4, 64, time.Minute, func() time.Time { return now })
	contentID, profileID := uuid.New(), uuid.New()
	v1 := compiledCacheKey{
		contentKind: "campaign", contentID: contentID, contentVersion: 1,
		profileID: profileID, profileVersion: 1,
	}
	cache.put(v1, mailrender.Compiled{HTML: "version one"})
	if _, ok := cache.get(v1); !ok {
		t.Fatal("fresh immutable version was not cached")
	}
	v2 := v1
	v2.contentVersion = 2
	cache.put(v2, mailrender.Compiled{HTML: "version two"})
	if _, ok := cache.get(v1); ok {
		t.Fatal("superseded content version remained cached")
	}
	cache.invalidateContent(contentID)
	if _, ok := cache.get(v2); ok {
		t.Fatal("explicitly invalidated content remained cached")
	}
	cache.put(v2, mailrender.Compiled{HTML: "version two"})
	now = now.Add(time.Minute + time.Nanosecond)
	if _, ok := cache.get(v2); ok {
		t.Fatal("expired compiled content remained cached")
	}
}

func TestCompiledContentCacheRejectsOversizeEntriesAndEvictsByBytes(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	cache := newCompiledContentCache(10, 12, time.Minute, func() time.Time { return now })
	base := compiledCacheKey{contentKind: "campaign", profileID: uuid.New(), profileVersion: 1}
	oversize := base
	oversize.contentID = uuid.New()
	cache.put(oversize, mailrender.Compiled{HTML: "0123456789abc"})
	if entries, _ := cache.stats(); entries != 0 {
		t.Fatalf("oversize cache entries = %d, want 0", entries)
	}
	first, second := base, base
	first.contentID, second.contentID = uuid.New(), uuid.New()
	cache.put(first, mailrender.Compiled{HTML: "12345678"})
	cache.put(second, mailrender.Compiled{HTML: "abcdefgh"})
	entries, bytes := cache.stats()
	if entries != 1 || bytes != 8 {
		t.Fatalf("byte-bounded cache = (%d entries, %d bytes), want (1, 8)", entries, bytes)
	}
}
