package connectors

// reconcile.go — periodic background poller that catches external issues missed by webhooks.
//
// Security model: reconcileOnce runs without a principal (no manyforge.principal_id GUC set).
// The connector table is RLS-protected; all DB reads and writes in this file go through
// SECURITY DEFINER functions (migration 0044) that bypass RLS, mirroring the pattern used
// by expire_stale_approvals (0032) and ingest_connector_webhook (0042).
//
// DEFINERs called (all in migration 0044, except connector_webhook_context in 0043):
//   - list_connectors_due_for_reconcile(interval) — list enabled connectors past their stale window
//   - connector_webhook_context(uuid) — lookup sealed credential + tenancy (migration 0043)
//   - enqueue_connector_inbound_sync(uuid,text) — outbox INSERT (derives tenancy from connector_id), RLS-exempt
//   - stamp_connector_reconciled(uuid) — UPDATE last_reconciled_at, RLS-exempt
//
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
	"github.com/manyforge/manyforge/internal/platform/db"
)

// dueConnector holds the fields returned by list_connectors_due_for_reconcile.
// Tenancy/type/credential are re-derived per-connector via connector_webhook_context,
// so the sweep query only needs the id + the reconcile cursor.
type dueConnector struct {
	ID               uuid.UUID
	LastReconciledAt pgtype.Timestamptz
}

// Reconciler is a periodic background goroutine that lists connectors whose
// last_reconciled_at is older than StaleAfter (or NULL = never reconciled),
// fetches the list of externally updated issues, and enqueues one
// connector.inbound.sync event per issue. The actual fetch+upsert is handled
// by InboundSyncSubscriber (decoupled).
type Reconciler struct {
	DB         *db.DB
	Sealer     *crypto.Sealer
	Registry   *Registry
	Logger     *slog.Logger
	Every      time.Duration
	StaleAfter time.Duration
}

// Run starts the reconcile ticker loop. It mirrors the approval-expire sweep pattern
// in cmd/manyforge/main.go:448-467: errors are logged but not fatal; the loop
// continues until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	// Zero-value guards: an unset Every would panic time.NewTicker; an unset
	// StaleAfter would make every connector perpetually due. T6's main.go wiring
	// passes these explicitly — these defaults are just a footgun backstop.
	if r.Every <= 0 {
		r.Every = time.Minute
	}
	if r.StaleAfter <= 0 {
		r.StaleAfter = 5 * time.Minute
	}
	t := time.NewTicker(r.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.reconcileOnce(ctx); err != nil {
				r.logger().WarnContext(ctx, "connectors/reconcile: pass failed", "err", err)
			}
		}
	}
}

// reconcileOnce performs a single reconcile pass: list due connectors via DEFINER,
// then for each independently list updated external issues and enqueue inbound-sync
// events. Per-connector errors are logged and do not abort the pass.
func (r *Reconciler) reconcileOnce(ctx context.Context) error {
	// Step 1: list connectors due for reconcile via SECURITY DEFINER
	// (connector table has RLS; principal-less tx sees nothing without the DEFINER).
	interval := pgtype.Interval{
		Microseconds: r.StaleAfter.Microseconds(),
		Valid:        true,
	}

	var due []dueConnector
	if err := r.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, last_reconciled_at
			   FROM list_connectors_due_for_reconcile($1)`,
			interval,
		)
		if err != nil {
			return fmt.Errorf("list_connectors_due_for_reconcile: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c dueConnector
			if err := rows.Scan(&c.ID, &c.LastReconciledAt); err != nil {
				return fmt.Errorf("scan due connector: %w", err)
			}
			due = append(due, c)
		}
		return rows.Err()
	}); err != nil {
		return fmt.Errorf("connectors/reconcile: list due connectors: %w", err)
	}

	r.logger().InfoContext(ctx, "connectors/reconcile: pass start", "due", len(due))

	// Step 2: process each connector independently (one failure logs + continues).
	for _, c := range due {
		if err := r.reconcileConnector(ctx, c); err != nil {
			r.logger().WarnContext(ctx, "connectors/reconcile: connector failed",
				"connector_id", c.ID, "err", err)
			// Continue to next connector.
		}
	}

	return nil
}

// reconcileConnector performs a single-connector reconcile: lookup context → unseal →
// list updated issues → enqueue events → stamp last_reconciled_at.
func (r *Reconciler) reconcileConnector(ctx context.Context, c dueConnector) error {
	// Step 2a: principal-less DEFINER lookup for connector context (migration 0043).
	// business_id + tenant_root_id are part of the DEFINER row but unused here — enqueue
	// derives them from connector_id internally — so they're scanned into discards.
	var (
		discardBiz    uuid.UUID
		discardTenant uuid.UUID
		ctype         string
		baseURL       string
		allowPriv     bool
		sealed        string
		configRaw     []byte
	)
	if err := r.DB.WithTx(ctx, func(tx pgx.Tx) error {
		// connector_outbound_context returns the same connector context as
		// connector_webhook_context PLUS config (jsonb); reuse it so the reconcile can read
		// config.project_key and scope the search to that one project (not every project the
		// token can see).
		return tx.QueryRow(ctx,
			`SELECT business_id, tenant_root_id, ctype, base_url, allow_private_base_url, sealed_secret, config
			   FROM connector_outbound_context($1)`,
			c.ID,
		).Scan(&discardBiz, &discardTenant, &ctype, &baseURL, &allowPriv, &sealed, &configRaw)
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			r.logger().WarnContext(ctx, "connectors/reconcile: connector not found or disabled",
				"connector_id", c.ID)
			return nil
		}
		return fmt.Errorf("connector_webhook_context: %w", err)
	}

	// Step 2b: unseal credential. Never log sealed or plain.
	plain, err := r.Sealer.Open(sealed)
	if err != nil {
		// Log without the sealed value.
		r.logger().ErrorContext(ctx, "connectors/reconcile: unseal failed",
			"connector_id", c.ID)
		return fmt.Errorf("unseal connector %s credential: %w", c.ID, err)
	}
	var cred Credential
	if err := json.Unmarshal(plain, &cred); err != nil {
		r.logger().ErrorContext(ctx, "connectors/reconcile: credential unmarshal failed",
			"connector_id", c.ID)
		return nil // corruption / Create bug — not recoverable by retry
	}

	// Step 2c: build connector client. Pass config so the client can scope to project_key.
	var cfg map[string]any
	if len(configRaw) > 0 {
		_ = json.Unmarshal(configRaw, &cfg)
	}
	conn, err := r.Registry.BuildSystem(ResolvedConnector{
		ID:                  c.ID.String(),
		Type:                ctype,
		BaseURL:             baseURL,
		AllowPrivateBaseURL: allowPriv,
		Config:              cfg,
		Credential:          cred,
	})
	if err != nil {
		return fmt.Errorf("BuildSystem connector %s: %w", c.ID, err)
	}

	// Step 2d: determine since time.
	var since time.Time
	if c.LastReconciledAt.Valid {
		since = c.LastReconciledAt.Time
	}
	// zero time.Time → full pull

	// ListUpdatedSince is network I/O; do NOT hold a DB tx during this call.
	ids, err := conn.ListUpdatedSince(ctx, since)
	if err != nil {
		return fmt.Errorf("ListUpdatedSince connector %s: %w", c.ID, err)
	}

	// Step 2e: enqueue one inbound-sync event per issue + stamp reconciled — in one tx.
	// Both writes go through SECURITY DEFINER functions (migration 0044) since the
	// connector table (for stamp) and outbox table (for enqueue) are RLS-protected.
	if err := r.DB.WithTx(ctx, func(tx pgx.Tx) error {
		for _, extID := range ids {
			if _, err := tx.Exec(ctx,
				`SELECT enqueue_connector_inbound_sync($1, $2)`,
				c.ID, extID,
			); err != nil {
				return fmt.Errorf("enqueue %s: %w", extID, err)
			}
		}
		_, err := tx.Exec(ctx, `SELECT stamp_connector_reconciled($1)`, c.ID)
		return err
	}); err != nil {
		return fmt.Errorf("enqueue+stamp connector %s: %w", c.ID, err)
	}

	r.logger().InfoContext(ctx, "connectors/reconcile: connector done",
		"connector_id", c.ID, "enqueued", len(ids))
	return nil
}

// ReconcileOne runs an immediate, on-demand reconcile of a single connector (the
// "Sync now" management action). It does a FULL pull (since = zero) so the caller gets a
// complete re-sync regardless of last_reconciled_at; the inbound upsert is idempotent
// (external-wins, deduped by the unique (connector_id, external_id) index) and the search is
// project-scoped via config.project_key. Principal-less like the poller (DEFINER-gated) —
// callers MUST authorize connector ownership (RLS) first. A disabled/unknown connector is a
// no-op (logged inside reconcileConnector).
func (r *Reconciler) ReconcileOne(ctx context.Context, connectorID uuid.UUID) error {
	return r.reconcileConnector(ctx, dueConnector{ID: connectorID})
}

func (r *Reconciler) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}
