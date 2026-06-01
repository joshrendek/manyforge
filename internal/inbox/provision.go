package inbox

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/events"
)

// uniqueViolation is the PostgreSQL SQLSTATE for a unique-constraint violation.
const uniqueViolation = "23505"

// systemLocalpartLen is the number of hex characters of the keyed HMAC taken for a
// system address localpart. 16 hex chars = 64 bits of unguessable entropy — enough
// that an attacker cannot enumerate a business's inbound address, while keeping the
// address short. The full address is b-<16 hex>@<system domain>.
const systemLocalpartLen = 16

// ProvisionConfig configures the system-address Provisioner. SystemDomain is the
// platform-hosted domain auto-provisioned addresses live on (cfg.InboundSystemDomain);
// SystemKey is the HMAC key the localpart is derived from — purpose-separated from
// the reply-token/webhook/JWT secrets so a leak of one cannot forge the others, and
// so the derived address is unguessable without it.
type ProvisionConfig struct {
	SystemDomain string
	SystemKey    []byte
}

// Provisioner auto-provisions a business's zero-config system inbound address
// (FR-001) in response to the business.created outbox event. It is the inbox-side
// subscriber that decouples address provisioning from tenancy: tenancy emits
// business.created (it does NOT import inbox), and this Provisioner — registered on
// the event bus — does the INSERT in the worker's transaction.
//
// Idempotency (REQUIRED — outbox delivery is at-least-once): the localpart is
// derived DETERMINISTICALLY from the business id under SystemKey, so the same
// business always maps to the same address; a replayed event re-derives the
// identical address. The INSERT is a plain INSERT whose duplicate raises a unique
// violation (SQLSTATE 23505) on the (tenant_root_id, address) index, which the
// handler swallows as a no-op. (We deliberately do NOT use ON CONFLICT DO NOTHING:
// the handler runs in the principal-less outbox-worker tx, and ON CONFLICT makes
// PostgreSQL evaluate the table's RLS USING predicate against the proposed row to
// resolve the conflict — which, with no principal, sees zero authorized rows and
// rejects the INSERT with an RLS error. A plain INSERT only evaluates WITH CHECK
// (true), so it succeeds, and a true duplicate surfaces as catchable 23505.) The
// inbound_address table has only a UNIQUE (tenant_root_id, address) constraint (no
// per-business system guard), so a deterministic address + that unique index is the
// only race-free idempotency mechanism that does not require a new migration.
type Provisioner struct {
	db     dbExecutor
	cfg    ProvisionConfig
	logger *slog.Logger
}

// dbExecutor is the minimal surface the Provisioner needs from *db.DB: a
// transactional escape hatch for the direct (non-event) provisioning path. Inside
// the event Handler it uses the worker-supplied tx instead, so this is only the
// fallback for callers that want to provision outside an event.
type dbExecutor interface {
	WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

// NewProvisioner builds the system-address Provisioner.
func NewProvisioner(database dbExecutor, cfg ProvisionConfig, logger *slog.Logger) *Provisioner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provisioner{db: database, cfg: cfg, logger: logger}
}

// businessCreatedPayload is the business.created event payload tenancy enqueues.
type businessCreatedPayload struct {
	BusinessID   uuid.UUID `json:"business_id"`
	TenantRootID uuid.UUID `json:"tenant_root_id"`
}

// Handle is the events.Handler for the business.created topic. It runs INSIDE the
// outbox worker's (principal-less) transaction, so the system-address INSERT
// commits atomically with the event being marked processed. It is safe to run
// multiple times (at-least-once): the deterministic address + ON CONFLICT DO
// NOTHING makes a replay a no-op.
func (p *Provisioner) Handle(ctx context.Context, tx pgx.Tx, e events.Event) error {
	var payload businessCreatedPayload
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return fmt.Errorf("inbox: provision: unmarshal business.created payload: %w", err)
	}
	if payload.BusinessID == uuid.Nil || payload.TenantRootID == uuid.Nil {
		return fmt.Errorf("inbox: provision: business.created payload missing ids")
	}
	return p.provision(ctx, tx, payload.BusinessID, payload.TenantRootID)
}

// provision inserts the system inbound address for one business on the given tx.
// kind='system' with email_domain_id NULL (the schema CHECK requires NULL for
// system, NOT NULL for custom). The INSERT runs under the worker's principal-less
// tx, where inbound_address RLS evaluates WITH CHECK (true) — so the plain INSERT is
// permitted. A replay re-derives the identical address and the INSERT raises a
// unique violation (23505) on (tenant_root_id, address), which we swallow as an
// idempotent no-op.
//
// The INSERT is wrapped in its own savepoint (pgx nested Begin): a 23505 aborts only
// that inner savepoint, leaving the worker's outer tx (and the surrounding event
// savepoint) usable so the event can still be marked processed. Without the inner
// savepoint, the failed statement would poison the whole tx.
func (p *Provisioner) provision(ctx context.Context, tx pgx.Tx, businessID, tenantRootID uuid.UUID) error {
	address := p.systemAddress(businessID)
	sp, err := tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return fmt.Errorf("inbox: provision savepoint: %w", err)
	}
	_, err = sp.Exec(ctx, `
		INSERT INTO inbound_address (business_id, tenant_root_id, address, kind, email_domain_id)
		VALUES ($1, $2, $3, 'system', NULL)`,
		businessID, tenantRootID, address)
	if err != nil {
		_ = sp.Rollback(ctx) // release the aborted savepoint; the outer tx stays live
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			// Already provisioned (replay or concurrent delivery): idempotent no-op.
			return nil
		}
		return fmt.Errorf("inbox: provision system address: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("inbox: provision commit savepoint: %w", err)
	}
	return nil
}

// systemAddress derives the deterministic, unguessable system address for a
// business: b-<hex(HMAC-SHA256(SystemKey, businessID[16]))[:N]>@<SystemDomain>.
// Deterministic ⇒ idempotent under replay (same business ⇒ same address ⇒ ON
// CONFLICT no-op). Keyed HMAC ⇒ unguessable: without SystemKey an attacker cannot
// compute (or enumerate the short tail of) the address from the business id.
func (p *Provisioner) systemAddress(businessID uuid.UUID) string {
	mac := hmac.New(sha256.New, p.cfg.SystemKey)
	idBytes := businessID // array, 16 bytes
	mac.Write(idBytes[:])
	sum := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("b-%s@%s", sum[:systemLocalpartLen], p.cfg.SystemDomain)
}
