package inbox

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/db"
)

// Config holds the ingestion service's runtime knobs. ReplyTokenKey is the HMAC
// server key used to mint/verify the threading reply token (ticketing.Sign/VerifyReplyToken);
// AttachmentMaxBytes caps a single stored attachment; InboundSystemDomain is the
// platform-hosted domain that auto-provisioned system inbound addresses live on.
type Config struct {
	ReplyTokenKey       []byte
	AttachmentMaxBytes  int64
	InboundSystemDomain string
}

// IngestResult reports the outcome of ingesting one inbound message. TicketID and
// MessageID are the DB ids of the ticket and the ticket_message row. Created is
// true when a brand-new ticket was opened; Duplicate is true when the message was
// a replay of an already-ingested Message-ID (idempotent no-op, SC-002/FR-005).
type IngestResult struct {
	TicketID  uuid.UUID
	MessageID uuid.UUID // DB id of the ticket_message row
	Created   bool      // a new ticket was created
	Duplicate bool      // replay of an already-ingested message_id
}

// Service ingests inbound messages: it resolves the recipient to exactly one
// business, threads the message onto a ticket, upserts the requester, stores
// attachments, and enqueues the outbound notification — all through the audited,
// business-scoped SECURITY DEFINER path. The real implementation lands in the
// GREEN task (T024-T027); this is the RED-baseline stub.
type Service struct {
	db     *db.DB
	blob   blob.Store
	cfg    Config
	logger *slog.Logger
}

// NewService constructs the ingestion Service.
func NewService(database *db.DB, store blob.Store, cfg Config, logger *slog.Logger) *Service {
	return &Service{db: database, blob: store, cfg: cfg, logger: logger}
}

// Ingest resolves, threads, and persists one inbound message.
//
// RED stub: returns a not-implemented error. The GREEN task (T024-T027) wires the
// resolve_inbound_address / ingest_inbound_message DEFINER calls, requester upsert,
// attachment sniff+store, and outbox enqueue to turn the failing suite green.
func (s *Service) Ingest(ctx context.Context, msg RawMessage) (IngestResult, error) {
	return IngestResult{}, errors.New("inbox: Ingest not implemented")
}
