// Package crm owns the tenant-wide customer-relationship surface (spec 005): contacts
// and companies shared across every business in the tenant tree (the CRM lives above
// the support-desk seam). Services here take (ctx, principalID, businessID, …): the
// businessID is the tenant context from the URL (RLS already gates the principal's
// visibility of it), from which the tenant_root_id is resolved inside the WithPrincipal
// tx. Every dbgen query additionally filters on tenant_root_id, so ownership is enforced
// both by RLS and in SQL (dual enforcement) — unknown / foreign-tenant / soft-deleted
// all collapse to ErrNotFound (no existence oracle).
package crm

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Contact is the API view of a CRM contact. CompanyID/DisplayName are optional and
// omitted from JSON when nil; deleted_at is never surfaced (soft-deleted rows are
// excluded from reads).
type Contact struct {
	ID           uuid.UUID  `json:"id"`
	TenantRootID uuid.UUID  `json:"tenant_root_id"`
	PrimaryEmail string     `json:"primary_email"`
	DisplayName  *string    `json:"display_name,omitempty"`
	CompanyID    *uuid.UUID `json:"company_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ContactInput is the create/update payload. On update, a nil field is preserved
// (the dbgen COALESCE narg reads NULL as "unchanged"), so callers send only the
// fields they intend to change.
type ContactInput struct {
	PrimaryEmail string
	DisplayName  *string
	CompanyID    *uuid.UUID
}

// Company is the API view of a CRM company. Domain is optional (citext, nullable) and
// omitted from JSON when nil. Companies are tenant-wide (keyed on tenant_root_id) and
// carry no PII / soft-delete column — Delete is a hard delete.
type Company struct {
	ID           uuid.UUID `json:"id"`
	TenantRootID uuid.UUID `json:"tenant_root_id"`
	Name         string    `json:"name"`
	Domain       *string   `json:"domain,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CompanyInput is the create/update payload. On update, Name is sent through a COALESCE
// narg (NULLIF on empty so an omitted Name preserves the current value) and a nil Domain
// is preserved (NULL narg read as "unchanged"), so callers send only what they change.
type CompanyInput struct {
	Name   string
	Domain *string
}

// ActivityEntry is the API view of a single activity-timeline row (spec 005 Phase B).
// An entry records something that happened to a contact (an email arrived, a ticket
// opened, a note was written). Actor/SourceID are optional and omitted from JSON when
// nil; Metadata is a raw JSON blob passed through verbatim (omitted when empty).
type ActivityEntry struct {
	ID           uuid.UUID       `json:"id"`
	TenantRootID uuid.UUID       `json:"tenant_root_id"`
	BusinessID   uuid.UUID       `json:"business_id"`
	ContactID    uuid.UUID       `json:"contact_id"`
	Kind         string          `json:"kind"`
	OccurredAt   time.Time       `json:"occurred_at"`
	Actor        *string         `json:"actor,omitempty"`
	SourceType   string          `json:"source_type"`
	SourceID     *uuid.UUID      `json:"source_id,omitempty"`
	Summary      string          `json:"summary"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// ActivityInput is the record payload. SourceID-bearing events dedupe on
// (tenant_root_id, source_type, source_id, kind); a nil SourceID always inserts.
// Metadata is passed through to the jsonb column verbatim (nil ⇒ SQL NULL).
type ActivityInput struct {
	BusinessID uuid.UUID
	ContactID  uuid.UUID
	Kind       string
	OccurredAt time.Time
	Actor      *string
	SourceType string
	SourceID   *uuid.UUID
	Summary    string
	Metadata   []byte
}

// Page is a keyset-paginated result. NextCursor is an opaque token (nil = last page).
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor,omitempty"`
}
