// Package telemetry owns manyforge-p20's client registration and the principal-less batch ingest
// endpoint shared by the analytics (as0) and crash-reporting (zw2) epics.
//
// The auth model is the one established by internal/feedback: an unguessable PUBLISHABLE key
// ('mfk_') that is safe to embed in a client binary, plus an optional server-to-server signing
// SECRET ('mfs_') that is sealed at rest and surfaced exactly once at creation. Storage lives in
// internal/platform/timeseries.
package telemetry

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Kind values for a registered client. The kind selects which event table ingest writes to.
const (
	KindAnalytics = "analytics"
	KindCrash     = "crash"
)

// Client is a registered telemetry source (an app for crash, a site for analytics).
type Client struct {
	ID             uuid.UUID  `json:"id"`
	BusinessID     uuid.UUID  `json:"business_id"`
	TenantRootID   uuid.UUID  `json:"tenant_root_id"`
	Kind           string     `json:"kind"`
	Name           string     `json:"name"`
	PublishableKey string     `json:"publishable_key"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	// HasSecret reports whether a signing secret exists, without exposing it.
	HasSecret bool `json:"has_secret"`
	// Secret is the plaintext 'mfs_' signing secret. Populated ONLY by CreateClient, on the
	// single response that mints it; every other path leaves it empty.
	Secret string `json:"secret,omitempty"`
}

// AnalyticsEvent is one inbound analytics datum. occurred_at is client-supplied and therefore
// untrusted — it is clamped/validated before it reaches the database, and it never influences
// which partition the row lands in.
type AnalyticsEvent struct {
	OccurredAt time.Time `json:"occurred_at"`
	Name       string    `json:"name"`
	Props      any       `json:"props,omitempty"`
}

// CrashEvent is one inbound crash report.
type CrashEvent struct {
	OccurredAt time.Time `json:"occurred_at"`
	Platform   string    `json:"platform"`
	AppVersion *string   `json:"app_version,omitempty"`
	Signature  string    `json:"signature"`
	Payload    any       `json:"payload,omitempty"`
}

func toClient(c dbgen.TelemetryClient) Client {
	out := Client{
		ID:             c.ID,
		BusinessID:     c.BusinessID,
		TenantRootID:   c.TenantRootID,
		Kind:           c.Kind,
		Name:           c.Name,
		PublishableKey: c.PublishableKey,
		Status:         c.Status,
		CreatedAt:      c.CreatedAt,
		HasSecret:      c.SealedSecret != nil,
	}
	if c.RevokedAt.Valid {
		t := c.RevokedAt.Time
		out.RevokedAt = &t
	}
	return out
}

// mapErr converts driver and pg errors into the typed sentinels handlers branch on. Raw errors
// never reach a client: pg messages carry constraint names, which are column names.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("telemetry: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("telemetry: duplicate: %w", errs.ErrConflict)
	case errors.As(err, &pgErr) && pgErr.Code == "23514":
		return fmt.Errorf("telemetry: check violation: %w", errs.ErrValidation)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrForbidden):
		return err
	default:
		return fmt.Errorf("telemetry: %w", err)
	}
}
