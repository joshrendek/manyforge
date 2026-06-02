package blob

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob" // registers the file:// scheme (self-host default)
	_ "gocloud.dev/blob/s3blob"   // registers the s3:// scheme (S3-compatible, optional)
	"gocloud.dev/gcerrors"
)

// Store is the attachment object-storage abstraction (SL-E). Bytes live here;
// the DB row holds only the key, sniffed content type, and size.
type Store interface {
	Put(ctx context.Context, key string, content []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	Close() error
}

// Bucket wraps a gocloud.dev/blob bucket (fileblob | s3blob).
type Bucket struct{ b *blob.Bucket }

// Open opens the bucket addressed by url — e.g. file:///var/lib/manyforge/blobs
// (local FS, self-host default) or s3://bucket?region=us-east-1 (S3-compatible).
// For a bare file:// URL it sets create_dir so the directory materializes on the
// first write.
func Open(ctx context.Context, url string) (*Bucket, error) {
	if strings.HasPrefix(url, "file://") && !strings.Contains(url, "?") {
		url += "?create_dir=true"
	}
	b, err := blob.OpenBucket(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open blob bucket: %w", err)
	}
	return &Bucket{b: b}, nil
}

// Put stores content under key with the (already sniffed) content type.
func (s *Bucket) Put(ctx context.Context, key string, content []byte, contentType string) error {
	return s.b.WriteAll(ctx, key, content, &blob.WriterOptions{ContentType: contentType})
}

// Get reads the bytes stored under key.
func (s *Bucket) Get(ctx context.Context, key string) ([]byte, error) {
	return s.b.ReadAll(ctx, key)
}

// Delete removes the object at key. It is idempotent — a key that is already gone is
// success — so the redaction/erasure purge path (at-least-once outbox delivery) never
// fails on a re-delivered or double-purged blob.
func (s *Bucket) Delete(ctx context.Context, key string) error {
	if err := s.b.Delete(ctx, key); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
		return err
	}
	return nil
}

// Close releases the bucket.
func (s *Bucket) Close() error { return s.b.Close() }

// Key builds a tenant-scoped storage key so an object key never crosses tenants.
func Key(tenantRootID, businessID, ticketID, attachmentID uuid.UUID) string {
	return fmt.Sprintf("%s/%s/%s/%s", tenantRootID, businessID, ticketID, attachmentID)
}

// ErrUnsupportedType is returned when a sniffed content type is outside the allowlist.
var ErrUnsupportedType = errors.New("attachment content type not allowed")

// allowedTypes is the sniffed-MIME allowlist (FR-007/SC-007). The declared
// Content-Type header is NEVER consulted; only the bytes decide.
var allowedTypes = map[string]bool{
	"image/jpeg":      true,
	"image/png":       true,
	"image/gif":       true,
	"image/webp":      true,
	"application/pdf": true,
	"text/plain":      true,
	"application/zip": true,
}

// Sniff determines the content type from the first 512 bytes and validates it
// against the allowlist (FR-007). A spoofed declared type is irrelevant: a file
// whose bytes sniff outside the allowlist is rejected with ErrUnsupportedType
// before any row or object is written.
func Sniff(content []byte) (string, error) {
	ct := http.DetectContentType(content) // e.g. "image/png" or "text/plain; charset=utf-8"
	base := strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
	if !allowedTypes[base] {
		return "", fmt.Errorf("%q: %w", base, ErrUnsupportedType)
	}
	return base, nil
}
