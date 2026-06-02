package ticketing

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// attachmentPurgePayload is the consumer-owned decode of the attachment.purge outbox
// event. The producer (Service.RedactTicket) enqueues map{"blob_key": <key>}; the
// json tag MUST match that key exactly.
type attachmentPurgePayload struct {
	BlobKey string `json:"blob_key"`
}

// AttachmentPurgeSubscriber drains attachment.purge events and deletes the attachment
// object from blob storage out-of-band (T066/FR-014). Unlike the redact tx that
// enqueued it, this runs in the outbox worker's PRINCIPAL-LESS per-event tx — but it
// touches NO RLS tables (only object storage), so that is irrelevant here. It MUST be
// idempotent: delivery is at-least-once, and blob.Store.Delete treats an
// already-gone key as success, so a re-delivered purge is a no-op.
type AttachmentPurgeSubscriber struct {
	Blob blob.Store
}

// Handle deletes the blob named in the event. A real storage error is returned so the
// worker reschedules with backoff; an already-missing blob is success (idempotent).
func (s AttachmentPurgeSubscriber) Handle(ctx context.Context, _ pgx.Tx, e events.Event) error {
	var p attachmentPurgePayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return fmt.Errorf("ticketing: attachment.purge: unmarshal payload: %w", err)
	}
	if p.BlobKey == "" {
		return nil // nothing to purge
	}
	if err := s.Blob.Delete(ctx, p.BlobKey); err != nil {
		return fmt.Errorf("ticketing: attachment.purge: delete %q: %w", p.BlobKey, err)
	}
	return nil
}
