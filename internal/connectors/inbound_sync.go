package connectors

// inbound_sync.go — outbox subscriber for connector.inbound.sync events.
//
// Security model: this subscriber runs inside the outbox worker's principal-less
// transaction (no manyforge.principal_id GUC set). All DB writes go through
// SECURITY DEFINER functions (sync_inbound_external_issue,
// sync_inbound_external_comment) that bypass RLS, mirroring the webhook handler.
// The sealed credential is NEVER logged.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	appdb "github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/events"
)

// inboundSyncPayload is the consumer-owned decode of the connector.inbound.sync
// outbox event produced by ingest_connector_webhook.
type inboundSyncPayload struct {
	ConnectorID uuid.UUID `json:"connector_id"`
	ExternalID  string    `json:"external_id"`
	BusinessID  uuid.UUID `json:"business_id"`
}

// InboundSyncSubscriber consumes connector.inbound.sync outbox events, fetches
// the external issue via the connector's typed client, and upserts the native
// ticket + requester + messages + sync_state through SECURITY DEFINER functions.
// It is principal-less: the worker tx has no manyforge.principal_id GUC.
type InboundSyncSubscriber struct {
	DB       *appdb.DB
	Sealer   *crypto.Sealer
	Registry *Registry
	Logger   *slog.Logger
}

// Handle implements events.Handler. It runs in the caller's (worker) tx.
func (s *InboundSyncSubscriber) Handle(ctx context.Context, tx pgx.Tx, e events.Event) error {
	// Step 1: decode payload. Bad payload → log + nil (poison; no reschedule).
	var p inboundSyncPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		s.logger().ErrorContext(ctx, "connectors/inbound_sync: bad payload",
			"event_id", e.ID, "err", err)
		return nil
	}
	if p.ConnectorID == uuid.Nil || p.ExternalID == "" {
		s.logger().ErrorContext(ctx, "connectors/inbound_sync: missing connector_id or external_id",
			"event_id", e.ID)
		return nil
	}

	// Step 2: DEFINER lookup (connector_webhook_context). ErrNoRows = connector
	// disabled or deleted → log + nil (don't reschedule forever).
	var (
		bizID      uuid.UUID
		tenantRoot uuid.UUID
		ctype      string
		baseURL    string
		allowPriv  bool
		sealed     string
	)
	err := tx.QueryRow(ctx,
		`SELECT business_id, tenant_root_id, ctype, base_url, allow_private_base_url, sealed_secret
		   FROM connector_webhook_context($1)`,
		p.ConnectorID,
	).Scan(&bizID, &tenantRoot, &ctype, &baseURL, &allowPriv, &sealed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.logger().WarnContext(ctx, "connectors/inbound_sync: connector not found or disabled",
				"connector_id", p.ConnectorID, "event_id", e.ID)
			return nil
		}
		return err
	}

	// Step 3: unseal credential (AES-256-GCM, Go-side). Never log sealed or plain.
	plain, err := s.Sealer.Open(sealed)
	if err != nil {
		s.logger().ErrorContext(ctx, "connectors/inbound_sync: unseal failed",
			"connector_id", p.ConnectorID, "event_id", e.ID)
		// RESCHEDULE (return err), don't drop: a rotated/misconfigured master key would
		// otherwise silently lose every inbound event forever. Noisy capped retries give
		// ops a window to fix the key before the outbox caps attempts. (Corrupt ciphertext
		// — essentially impossible post-GCM — would loop until the cap; acceptable vs data loss.)
		return fmt.Errorf("connectors/inbound_sync: unseal connector %s credential: %w", p.ConnectorID, err)
	}
	var cred Credential
	if err := json.Unmarshal(plain, &cred); err != nil {
		// Unlike unseal: a GCM-authenticated plaintext was written by Service.Create, which
		// always marshals a valid Credential — so this is genuine corruption/a Create bug,
		// not recoverable by retry. Drop (return nil).
		s.logger().ErrorContext(ctx, "connectors/inbound_sync: credential unmarshal failed",
			"connector_id", p.ConnectorID, "event_id", e.ID)
		return nil
	}

	// Step 4: build connector client (principal-less factory). Error = config/registry
	// issue → return for reschedule (transient).
	conn, err := s.Registry.BuildSystem(ResolvedConnector{
		ID:                  p.ConnectorID.String(),
		Type:                ctype,
		BaseURL:             baseURL,
		AllowPrivateBaseURL: allowPriv,
		Credential:          cred,
	})
	if err != nil {
		return err
	}

	// Step 5: fetch external issue (network I/O inside worker tx — acceptable, mirrors
	// notify.SendSubscriber). Error = transient HTTP → return for reschedule.
	iss, err := conn.FetchIssue(ctx, p.ExternalID)
	if err != nil {
		return err
	}

	// Step 6: build snapshot JSON. status/priority/subject are read back on the next sync
	// to detect both-changed conflicts (manyforge-a7j.9); updated_at is informational.
	snapshot, _ := json.Marshal(map[string]any{
		"status":     iss.Status,
		"priority":   iss.Priority,
		"subject":    iss.Title,
		"updated_at": iss.UpdatedAt,
	})

	// Step 7: external-wins upsert of requester + ticket + sync_state.
	var ticketID uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT sync_inbound_external_issue($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		p.ConnectorID,
		p.ExternalID,
		iss.URL,
		iss.Title,
		iss.Status,
		iss.Priority,
		iss.ReporterEmail,
		iss.ReporterName,
		iss.UpdatedAt,
		snapshot,
	).Scan(&ticketID); err != nil {
		return err
	}

	// Step 8a: sync the issue's main body (Jira `description`) as an inbound message (the
	// original request body), so a description-only issue (no comments) still produces a
	// readable inbound message — without it the agent/UI see only the subject. Reuses the
	// comment DEFINER with a stable synthetic external_id (<ExternalID>:description) so it's
	// deduped on every reconcile and can never collide with a numeric Jira comment id.
	// The issue's created time is threaded through as the message created_at so the description
	// sorts first against the comments by real time, not the shared reconcile now() (manyforge-4d1).
	if iss.Description != "" {
		var descMsgID pgtype.UUID
		if err := tx.QueryRow(ctx,
			`SELECT sync_inbound_external_comment($1,$2,$3,$4,$5)`,
			ticketID, p.ConnectorID, iss.ExternalID+":description", iss.Description, inboundCreatedAt(iss.CreatedAt),
		).Scan(&descMsgID); err != nil {
			return err
		}
		// descMsgID.Valid==false means dedupe (already synced) — that is fine.
	}

	// Step 8b: append-only comment upsert (dedupe via connector_id+external_id). Each comment's
	// real created time is threaded through so the thread sorts chronologically (manyforge-4d1).
	for _, c := range iss.Comments {
		var msgID pgtype.UUID
		if err := tx.QueryRow(ctx,
			`SELECT sync_inbound_external_comment($1,$2,$3,$4,$5)`,
			ticketID, p.ConnectorID, c.ExternalID, c.Body, inboundCreatedAt(c.CreatedAt),
		).Scan(&msgID); err != nil {
			return err
		}
		// msgID.Valid==false means dedupe (already seen) — that is fine.
	}

	s.logger().InfoContext(ctx, "connectors/inbound_sync: synced",
		"connector_id", p.ConnectorID, "external_id", p.ExternalID,
		"ticket_id", ticketID, "comments", len(iss.Comments))
	return nil
}

// inboundCreatedAt maps an external timestamp to the sync_inbound_external_comment p_created_at
// argument. A zero time (the connector didn't expose one) becomes SQL NULL so the DEFINER's
// COALESCE(p_created_at, now()) falls back to the insert time (manyforge-4d1).
func inboundCreatedAt(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

func (s *InboundSyncSubscriber) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
