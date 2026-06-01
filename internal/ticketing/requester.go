package ticketing

import (
	"time"

	"github.com/google/uuid"
)

// Requester is a tenant-scoped external sender (T026), deduped by email within the
// tenant and never a principal/account (FR-006). It is the read-side projection of
// the requester table shared by the ingest path and the read slice (US1 GET
// /requesters). The ingest-side upsert itself lives INSIDE the
// ingest_inbound_message SECURITY DEFINER function (migration 0014) — it runs the
// `INSERT … ON CONFLICT (tenant_root_id, email) DO UPDATE SET last_seen_at=now(),
// display_name=COALESCE(EXCLUDED.display_name, …)` dedup in the same principal-less
// transaction — so there is intentionally NO Go-side upsert here; duplicating it
// would risk drift from the audited DEFINER path.
type Requester struct {
	ID           uuid.UUID
	TenantRootID uuid.UUID
	Email        string

	// DisplayName is the most recent non-empty sender display name, or nil when the
	// sender never supplied one.
	DisplayName *string

	// ContactID is the reserved CRM-contact seam (spec 005): the requester table
	// carries a nullable contact_id with NO foreign key yet, so a later CRM slice
	// can link a requester to a contact without a migration. It is always nil in
	// this slice and is surfaced (never omitted) so the API/read layer projects the
	// seam consistently rather than inventing it later.
	ContactID *uuid.UUID

	FirstSeenAt time.Time
	LastSeenAt  time.Time
}
