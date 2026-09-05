package automations

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

const (
	maxEventPropertiesBytes = 65536
	maxEventFutureSkew       = 5 * time.Minute
)

func (s *Service) CreateEvent(ctx context.Context, principalID, businessID uuid.UUID, input EventInput) (EventView, error) {
	input, properties, err := prepareEvent(input)
	if err != nil {
		return EventView{}, err
	}
	var out EventView
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		root, err := resolveTenantRoot(ctx, dbgen.New(tx), businessID)
		if err != nil {
			return err
		}
		if err = requirePermission(ctx, tx, principalID, businessID, root, "mailing.send"); err != nil {
			return err
		}
		var listID *uuid.UUID
		if input.SubscriberID != nil {
			var resolvedList uuid.UUID
			var resolvedEmail string
			if err = tx.QueryRow(ctx, `SELECT list_id,email FROM list_subscriber
				WHERE id=$1 AND business_id=$2 AND tenant_root_id=$3`, *input.SubscriberID, businessID, root).Scan(&resolvedList, &resolvedEmail); err != nil {
				return err
			}
			listID = &resolvedList
			input.Email = &resolvedEmail
		}
		out, _, err = ingestEventTx(ctx, tx, businessID, root, listID, nil, nil, input, properties)
		if err != nil {
			return err
		}
		return writeAuditTarget(ctx, tx, principalID, businessID, root, "automation.event.received", "automation_event", out.ID,
			map[string]any{"name": out.Name, "created": out.Created})
	})
	return out, mapErr(err)
}

// IngestS2SEvent implements mailing.S2SEventIngestor without importing the mailing package.
// Authentication has already resolved the key's exact business, tenant root, and list, and this
// method executes inside the verifier's transaction.
func (s *Service) IngestS2SEvent(ctx context.Context, tx pgx.Tx, businessID, tenantRootID, listID, keyID uuid.UUID, raw []byte) (any, bool, error) {
	input, err := decodeEventInput(raw)
	if err != nil {
		return nil, false, err
	}
	input, properties, err := prepareEvent(input)
	if err != nil {
		return nil, false, err
	}
	if input.IdempotencyKey == nil {
		return nil, false, validation("idempotency_key is required")
	}
	if input.SubscriberID != nil {
		var resolvedEmail string
		if err = tx.QueryRow(ctx, `SELECT email FROM automation_resolve_event_subscriber($1,$2,$3,$4)`,
			businessID, tenantRootID, listID, *input.SubscriberID).Scan(&resolvedEmail); err != nil {
			return nil, false, mapErr(err)
		}
		input.Email = &resolvedEmail
	}
	fingerprint, err := eventRequestFingerprint(input, properties)
	if err != nil {
		return nil, false, err
	}
	out, storedFingerprint, err := ingestEventTx(
		ctx, tx, businessID, tenantRootID, &listID, &keyID, fingerprint, input, properties,
	)
	if err != nil {
		return nil, false, mapErr(err)
	}
	if subtle.ConstantTimeCompare(storedFingerprint, fingerprint) != 1 {
		return nil, false, errs.ErrConflict
	}
	return out, out.Created, nil
}

func ingestEventTx(
	ctx context.Context,
	tx pgx.Tx,
	businessID, tenantRootID uuid.UUID,
	listID, ingressKeyID *uuid.UUID,
	fingerprint []byte,
	input EventInput,
	properties []byte,
) (EventView, []byte, error) {
	var out EventView
	var listArg, keyArg, subscriberArg pgtype.UUID
	if listID != nil {
		listArg = pgtype.UUID{Bytes: *listID, Valid: true}
	}
	if ingressKeyID != nil {
		keyArg = pgtype.UUID{Bytes: *ingressKeyID, Valid: true}
	}
	if input.SubscriberID != nil {
		subscriberArg = pgtype.UUID{Bytes: *input.SubscriberID, Valid: true}
	}
	var occurredArg pgtype.Timestamptz
	if input.OccurredAt != nil {
		occurredArg = pgtype.Timestamptz{Time: input.OccurredAt.UTC(), Valid: true}
	}
	var idempotencyArg pgtype.Text
	if input.IdempotencyKey != nil {
		idempotencyArg = pgtype.Text{String: *input.IdempotencyKey, Valid: true}
	}
	var fingerprintArg []byte
	if len(fingerprint) > 0 {
		fingerprintArg = fingerprint
	}
	var storedSubscriber pgtype.UUID
	var storedIdempotency pgtype.Text
	var storedFingerprint []byte
	err := tx.QueryRow(ctx, `SELECT event_id,event_business_id,event_name,event_email,event_subscriber_id,
		event_occurred_at,event_idempotency_key,event_properties,event_created_at,event_request_fingerprint,was_created
		FROM automation_ingest_event($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		businessID, tenantRootID, listArg, keyArg, fingerprintArg, input.Name, *input.Email,
		subscriberArg, occurredArg, properties, idempotencyArg,
	).Scan(&out.ID, &out.BusinessID, &out.Name, &out.Email, &storedSubscriber, &out.OccurredAt,
		&storedIdempotency, &out.Properties, &out.CreatedAt, &storedFingerprint, &out.Created)
	out.SubscriberID = uuidPtr(storedSubscriber)
	out.IdempotencyKey = textPtr(storedIdempotency)
	return out, storedFingerprint, err
}

func decodeEventInput(raw []byte) (EventInput, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input EventInput
	if err := decoder.Decode(&input); err != nil {
		return EventInput{}, validation("invalid event body")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return EventInput{}, validation("invalid event body")
	}
	return input, nil
}

func prepareEvent(input EventInput) (EventInput, []byte, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 128 {
		return EventInput{}, nil, validation("name is required and must not exceed 128 characters")
	}
	if (input.Email == nil) == (input.SubscriberID == nil) {
		return EventInput{}, nil, validation("exactly one of email or subscriber_id is required")
	}
	if input.Email != nil {
		email, err := normalizeEventEmail(*input.Email)
		if err != nil {
			return EventInput{}, nil, err
		}
		input.Email = &email
	}
	if input.IdempotencyKey != nil {
		key := strings.TrimSpace(*input.IdempotencyKey)
		if key == "" || len(key) > 200 {
			return EventInput{}, nil, validation("idempotency_key must contain 1 to 200 characters")
		}
		input.IdempotencyKey = &key
	}
	if input.OccurredAt != nil {
		occurredAt := input.OccurredAt.UTC()
		if occurredAt.After(time.Now().UTC().Add(maxEventFutureSkew)) {
			return EventInput{}, nil, validation("occurred_at is too far in the future")
		}
		input.OccurredAt = &occurredAt
	}
	if input.Properties == nil {
		input.Properties = map[string]any{}
	}
	properties, err := json.Marshal(input.Properties)
	if err != nil || len(properties) > maxEventPropertiesBytes {
		return EventInput{}, nil, validation("properties must be a JSON object no larger than 65536 bytes")
	}
	return input, properties, nil
}

func eventRequestFingerprint(input EventInput, properties []byte) ([]byte, error) {
	canonical := struct {
		Name           string          `json:"name"`
		Email          *string         `json:"email"`
		SubscriberID   *uuid.UUID      `json:"subscriber_id"`
		OccurredAt     *time.Time      `json:"occurred_at"`
		IdempotencyKey *string         `json:"idempotency_key"`
		Properties     json.RawMessage `json:"properties"`
	}{
		Name: input.Name, Email: input.Email, SubscriberID: input.SubscriberID,
		OccurredAt: input.OccurredAt, IdempotencyKey: input.IdempotencyKey,
		Properties: json.RawMessage(properties),
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return nil, validation("invalid event body")
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

func normalizeEventEmail(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := mail.ParseAddress(value)
	if err != nil || !strings.EqualFold(parsed.Address, value) || len(value) > 320 {
		return "", validation("invalid email")
	}
	return strings.ToLower(parsed.Address), nil
}
