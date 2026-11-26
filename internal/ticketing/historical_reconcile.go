package ticketing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrHistoricalReconciliation = errors.New("restored records do not match historical snapshot")

// ReconcileHistoricalTicket checks the persisted snapshot checksum, archive
// entries, and attachment bytes. It ignores ordinary replies added after import.
func (s *PortalService) ReconcileHistoricalTicket(ctx context.Context, principal, business, ticket uuid.UUID, in HistoricalTicket) error {
	if err := ValidateHistoricalTicket(in); err != nil {
		return err
	}
	digest, err := historicalDigest(in)
	if err != nil {
		return err
	}
	return s.DB.WithPrincipal(ctx, principal, func(tx pgx.Tx) error {
		if err := s.allowed(ctx, tx, principal, business, true); err != nil {
			return err
		}
		var storedDigest []byte
		var root uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT historical_hash, tenant_root_id FROM ticket
			WHERE id=$1 AND business_id=$2 AND historical_source=$3 AND historical_id=$4 AND redacted_at IS NULL
			FOR SHARE`, ticket, business, in.Source, in.ID).Scan(&storedDigest, &root)
		if err != nil {
			return portalNotFound(err)
		}
		if !bytes.Equal(storedDigest, digest) {
			return ErrHistoricalReconciliation
		}
		if err := reconcileHistoricalMessages(ctx, tx, business, root, ticket, in.Messages); err != nil {
			return err
		}
		return s.reconcileHistoricalAttachments(ctx, tx, business, root, ticket, in.Messages)
	})
}

func reconcileHistoricalMessages(ctx context.Context, tx pgx.Tx, business, root, ticket uuid.UUID, messages []HistoricalMessage) error {
	expected := make(map[string]HistoricalMessage, len(messages))
	for _, message := range messages {
		expected[message.ID] = message
	}
	rows, err := tx.Query(ctx, `
		SELECT historical_id, direction::text, COALESCE(body_text,''), created_at
		FROM ticket_message WHERE ticket_id=$1 AND business_id=$2 AND tenant_root_id=$3 AND historical_id IS NOT NULL`,
		ticket, business, root)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, direction, body string
		var created time.Time
		if err := rows.Scan(&id, &direction, &body, &created); err != nil {
			return err
		}
		want, ok := expected[id]
		if !ok || direction != want.Direction || body != want.Body || !created.Equal(want.CreatedAt.Truncate(time.Microsecond)) {
			return ErrHistoricalReconciliation
		}
		delete(expected, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(expected) > 0 {
		return ErrHistoricalReconciliation
	}
	return nil
}

func (s *PortalService) reconcileHistoricalAttachments(ctx context.Context, tx pgx.Tx, business, root, ticket uuid.UUID, messages []HistoricalMessage) error {
	type identity struct{ message, file string }
	expected := map[identity]preparedAttachment{}
	for _, message := range messages {
		for _, file := range message.Attachments {
			expected[identity{message.ID, file.ID}] = prepareHistoricalAttachment(message.Direction, file)
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT m.historical_id,a.historical_id,COALESCE(a.filename,''),a.content_type,a.size,a.staff_only,a.created_at,a.blob_key
		FROM ticket_message m JOIN attachment a
			ON a.ticket_message_id=m.id AND a.business_id=m.business_id AND a.tenant_root_id=m.tenant_root_id
		WHERE m.ticket_id=$1 AND m.business_id=$2 AND m.tenant_root_id=$3 AND a.historical_id IS NOT NULL`,
		ticket, business, root)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id identity
		var filename, contentType, key string
		var size int64
		var staffOnly bool
		var created time.Time
		if err := rows.Scan(&id.message, &id.file, &filename, &contentType, &size, &staffOnly, &created, &key); err != nil {
			return err
		}
		want, ok := expected[id]
		if !ok || filename != want.Filename || contentType != want.contentType || size != int64(len(want.Content)) ||
			staffOnly != want.staffOnly || !created.Equal(want.createdAt.Truncate(time.Microsecond)) {
			return ErrHistoricalReconciliation
		}
		if err := s.reconcileHistoricalBlob(ctx, key, want.Content); err != nil {
			return err
		}
		delete(expected, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(expected) > 0 {
		return ErrHistoricalReconciliation
	}
	return nil
}

func (s *PortalService) reconcileHistoricalBlob(ctx context.Context, key string, expected []byte) error {
	if s.Blob == nil {
		return ErrAttachmentUnavailable
	}
	reader, size, err := s.Blob.OpenReader(ctx, key)
	if err != nil {
		return ErrAttachmentUnavailable
	}
	defer func() { _ = reader.Close() }()
	if size != int64(len(expected)) {
		return ErrHistoricalReconciliation
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(reader, size+1))
	if err != nil {
		return ErrAttachmentUnavailable
	}
	want := sha256.Sum256(expected)
	if n != size || !bytes.Equal(h.Sum(nil), want[:]) {
		return ErrHistoricalReconciliation
	}
	return nil
}
