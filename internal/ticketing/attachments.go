package ticketing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

var ErrAttachmentUnavailable = errors.New("support attachment unavailable")

const (
	maxHistoricalFileBytes = 64 << 20
	maxAttachmentJSONBytes = int64(4*((blob.MaxSupportUploadBytes+2)/3) + (1 << 20))
)

// AttachmentInput is a file to attach to a new support message.
type AttachmentInput struct {
	Filename string `json:"filename"`
	Content  []byte `json:"content"`
}

type preparedAttachment struct {
	AttachmentInput
	contentType  string
	staffOnly    bool
	historicalID *string
	createdAt    time.Time
}

func validAttachmentName(name string) bool {
	return name != "." && name != ".." && portalText(name, 1020) &&
		utf8.RuneCountInString(name) <= 255 && !strings.ContainsAny(name, "/\\") &&
		strings.IndexFunc(name, unicode.IsControl) < 0
}

func prepareLiveAttachments(inputs []AttachmentInput) ([]preparedAttachment, error) {
	if len(inputs) > blob.MaxSupportUploadFiles {
		return nil, errs.ErrValidation
	}
	prepared := make([]preparedAttachment, 0, len(inputs))
	total := 0
	for _, file := range inputs {
		total += len(file.Content)
		if !validAttachmentName(file.Filename) || len(file.Content) == 0 || total > blob.MaxSupportUploadBytes {
			return nil, errs.ErrValidation
		}
		contentType, err := blob.Sniff(file.Content)
		if err != nil {
			return nil, errs.ErrValidation
		}
		prepared = append(prepared, preparedAttachment{AttachmentInput: file, contentType: contentType})
	}
	return prepared, nil
}

type attachmentBatch struct {
	store   blob.Store
	written []string
}

func (b *attachmentBatch) add(ctx context.Context, tx pgx.Tx, root, business, ticket, message uuid.UUID, file preparedAttachment) (Attachment, error) {
	if b.store == nil {
		return Attachment{}, ErrAttachmentUnavailable
	}
	id := uuid.New()
	key := blob.Key(root, business, ticket, id)
	created := file.createdAt
	if created.IsZero() {
		created = time.Now()
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO attachment(id,ticket_message_id,business_id,tenant_root_id,blob_key,filename,content_type,size,created_at,staff_only,historical_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		id, message, business, root, key, file.Filename, file.contentType, len(file.Content), created, file.staffOnly, file.historicalID); err != nil {
		return Attachment{}, err
	}
	b.written = append(b.written, key)
	if err := b.store.Put(ctx, key, file.Content, file.contentType); err != nil {
		return Attachment{}, fmt.Errorf("%w: file write failed", ErrAttachmentUnavailable)
	}
	return Attachment{ID: id, Filename: file.Filename, BlobKey: key, ContentType: file.contentType, Size: int64(len(file.Content))}, nil
}

// discard is used for a known transaction failure before commit. An ambiguous
// commit must retain the objects because a retry can find committed rows.
func (b *attachmentBatch) discard(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for _, key := range b.written {
		if err := b.store.Delete(ctx, key); err != nil {
			slog.WarnContext(ctx, "support attachment rollback cleanup failed")
		}
	}
}

func serveSupportAttachment(w http.ResponseWriter, a Attachment, content io.ReadCloser) {
	defer func() { _ = content.Close() }()
	filename := a.Filename
	if !validAttachmentName(filename) {
		filename = "attachment"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(a.Size))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.CopyN(w, content, a.Size)
}

func readSupportAttachment(ctx context.Context, tx pgx.Tx, store blob.StreamingStore, business, ticket, attachment uuid.UUID, customer bool) (Attachment, io.ReadCloser, error) {
	var file Attachment
	err := tx.QueryRow(ctx, `
		SELECT a.id,COALESCE(a.filename,''),a.content_type,a.size,a.blob_key
		FROM attachment a
		JOIN ticket_message m ON m.id=a.ticket_message_id AND m.business_id=a.business_id AND m.tenant_root_id=a.tenant_root_id
		JOIN ticket t ON t.id=m.ticket_id AND t.business_id=m.business_id AND t.tenant_root_id=m.tenant_root_id
		WHERE a.id=$1 AND a.business_id=$2 AND t.id=$3 AND t.redacted_at IS NULL
		  AND (NOT $4::boolean OR (m.direction IN ('inbound','outbound') AND NOT a.staff_only))
		FOR SHARE OF t`, attachment, business, ticket, customer).
		Scan(&file.ID, &file.Filename, &file.ContentType, &file.Size, &file.BlobKey)
	if err != nil {
		return Attachment{}, nil, portalNotFound(err)
	}
	if store == nil || file.Size <= 0 || file.Size > maxHistoricalFileBytes {
		return Attachment{}, nil, ErrAttachmentUnavailable
	}
	content, size, err := store.OpenReader(ctx, file.BlobKey)
	if err != nil {
		return Attachment{}, nil, ErrAttachmentUnavailable
	}
	if size != file.Size {
		_ = content.Close()
		return Attachment{}, nil, ErrAttachmentUnavailable
	}
	return file, content, nil
}

func (s *Service) DownloadAttachment(ctx context.Context, principal, business, ticket, attachment uuid.UUID) (Attachment, io.ReadCloser, error) {
	var file Attachment
	var content io.ReadCloser
	err := s.DB.WithPrincipal(ctx, principal, func(tx pgx.Tx) error {
		var err error
		file, content, err = readSupportAttachment(ctx, tx, s.Blob, business, ticket, attachment, false)
		return err
	})
	if err != nil && content != nil {
		_ = content.Close()
		return Attachment{}, nil, err
	}
	return file, content, err
}

func (s *PortalService) DownloadAttachment(ctx context.Context, scope PortalScope, ticket, attachment uuid.UUID) (Attachment, io.ReadCloser, error) {
	var file Attachment
	var content io.ReadCloser
	err := s.withScope(ctx, scope, func(tx pgx.Tx) error {
		if _, err := portalReadTicket(ctx, tx, scope, ticket); err != nil {
			return err
		}
		var err error
		file, content, err = readSupportAttachment(ctx, tx, s.Blob, scope.BusinessID, ticket, attachment, true)
		return err
	})
	if err != nil && content != nil {
		_ = content.Close()
		return Attachment{}, nil, err
	}
	return file, content, err
}
