package connectors

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// RecordWebhookDelivery records an inbound webhook delivery for replay protection, in
// the caller's tx. Returns true if newly recorded, false if this (connector, delivery_id)
// was already seen — or if the connector is not visible to the business (RLS/guard) — in
// which case the caller should no-op. The query's ON CONFLICT DO NOTHING + same-business
// EXISTS guard make replays and cross-business attempts both yield zero rows.
// Callers MUST resolve the connector (Registry.Resolve / Service.Resolve) first as the
// primary auth check; the same-business EXISTS guard here is defence-in-depth, not a
// substitute for it.
func (s *Service) RecordWebhookDelivery(ctx context.Context, tx pgx.Tx, businessID, connectorID uuid.UUID, deliveryID string) (bool, error) {
	// No mapErr: :execrows never returns pgx.ErrNoRows, and ON CONFLICT silences duplicates.
	n, err := dbgen.New(tx).RecordWebhookDelivery(ctx, dbgen.RecordWebhookDeliveryParams{
		ID:                 uuid.New(), // internal PK; callers never reference it
		BusinessID:         businessID,
		ConnectorID:        connectorID,
		ExternalDeliveryID: deliveryID,
	})
	if err != nil {
		return false, fmt.Errorf("connectors: record webhook delivery: %w", err)
	}
	return n > 0, nil
}
