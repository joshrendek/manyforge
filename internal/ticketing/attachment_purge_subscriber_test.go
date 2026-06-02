package ticketing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/events"
)

// stubPurgeStore is a blob.Store that records Delete calls (and can inject an error).
type stubPurgeStore struct {
	deleted []string
	err     error
}

func (s *stubPurgeStore) Put(context.Context, string, []byte, string) error { return nil }
func (s *stubPurgeStore) Get(context.Context, string) ([]byte, error)       { return nil, nil }
func (s *stubPurgeStore) Close() error                                      { return nil }
func (s *stubPurgeStore) Delete(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	return s.err
}

func purgeEvent(t *testing.T, payload any) events.Event {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return events.Event{ID: uuid.New(), Topic: events.TopicAttachmentPurge, Payload: b}
}

// TestAttachmentPurgeSubscriber covers the happy path, the idempotent/no-op cases, and
// error propagation for the redact attachment-purge consumer (T066).
func TestAttachmentPurgeSubscriber(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes the named blob", func(t *testing.T) {
		store := &stubPurgeStore{}
		sub := AttachmentPurgeSubscriber{Blob: store}
		if err := sub.Handle(ctx, nil, purgeEvent(t, map[string]any{"blob_key": "tenant/biz/ticket/att"})); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if len(store.deleted) != 1 || store.deleted[0] != "tenant/biz/ticket/att" {
			t.Errorf("deleted = %v, want [tenant/biz/ticket/att]", store.deleted)
		}
	})

	t.Run("empty blob_key is a no-op", func(t *testing.T) {
		store := &stubPurgeStore{}
		sub := AttachmentPurgeSubscriber{Blob: store}
		if err := sub.Handle(ctx, nil, purgeEvent(t, map[string]any{"blob_key": ""})); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if len(store.deleted) != 0 {
			t.Errorf("deleted = %v, want none", store.deleted)
		}
	})

	t.Run("storage error is returned for retry", func(t *testing.T) {
		sentinel := errors.New("s3 unavailable")
		store := &stubPurgeStore{err: sentinel}
		sub := AttachmentPurgeSubscriber{Blob: store}
		err := sub.Handle(ctx, nil, purgeEvent(t, map[string]any{"blob_key": "k"}))
		if !errors.Is(err, sentinel) {
			t.Errorf("Handle err = %v, want wrapped %v (worker must reschedule on a real storage error)", err, sentinel)
		}
	})

	t.Run("undecodable payload errors (bounded by worker MaxAttempts)", func(t *testing.T) {
		store := &stubPurgeStore{}
		sub := AttachmentPurgeSubscriber{Blob: store}
		bad := events.Event{ID: uuid.New(), Topic: events.TopicAttachmentPurge, Payload: []byte("{not json")}
		if err := sub.Handle(ctx, nil, bad); err == nil {
			t.Error("Handle(undecodable) = nil, want a decode error")
		}
		if len(store.deleted) != 0 {
			t.Errorf("deleted = %v, want none on undecodable payload", store.deleted)
		}
	})
}
