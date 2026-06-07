package connectors

// outbound.go — background poller that drains connector_outbound_op: posts native replies
// to the external system as comments and creates external issues from escalated tickets,
// then writes the resulting external_id back onto the native row.
//
// Security model (mirrors reconcile.go): runs WITHOUT a principal. connector_outbound_op +
// ticket + ticket_message are RLS-protected, so every queue read/write goes through the
// SECURITY DEFINER fns in migration 0045. The sealed credential is NEVER logged. The HTTP
// call is made with NO DB tx open (US4 note (b) / reconciler pattern): short tx for claim,
// short tx for context-load, NO tx during HTTP, short tx for write-back.
//
// DEFINERs called (migration 0045):
//   - claim_outbound_ops(int)                            — atomically claim pending ops (returns post-increment attempts)
//   - connector_outbound_context(uuid)                   — sealed credential + tenancy + config
//   - message_external_id(uuid)                          — idempotency read (RLS-protected ticket_message)
//   - complete_outbound_comment(uuid,uuid,uuid,text)     — stamp message external_id + mark op done + audit
//   - complete_outbound_create(uuid,uuid,uuid,text,text) — link ticket + mark op done + audit
//   - fail_outbound_op(uuid,text,boolean)                — retry (pending) / terminal (failed)

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

// maxOutboundAttempts caps retries before an op is marked terminally failed. claim_outbound_ops
// increments attempts on claim and returns the post-increment value, so an op fails terminally
// once it has been attempted maxOutboundAttempts times.
const maxOutboundAttempts = 5

// claimedOp is one row returned by claim_outbound_ops.
type claimedOp struct {
	ID            uuid.UUID
	Type          string
	ConnectorID   uuid.UUID
	TicketID      uuid.UUID
	MessageID     pgtype.UUID
	TicketExtID   *string
	TicketSubject string
	Body          *string
	Attempts      int
}

// OutboundDispatcher periodically claims pending outbound ops and pushes them to the
// external system, writing external ids back. Modeled on Reconciler.
type OutboundDispatcher struct {
	DB       *db.DB
	Sealer   *crypto.Sealer
	Registry *Registry
	Logger   *slog.Logger
	Every    time.Duration
	Batch    int
}

// Run starts the dispatch ticker loop. Errors are logged, never fatal; the loop continues
// until ctx is cancelled. Mirrors Reconciler.Run.
func (d *OutboundDispatcher) Run(ctx context.Context) {
	if d.Every <= 0 {
		d.Every = 15 * time.Second
	}
	if d.Batch <= 0 {
		d.Batch = 20
	}
	t := time.NewTicker(d.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.dispatchOnce(ctx); err != nil {
				d.logger().WarnContext(ctx, "connectors/outbound: pass failed", "err", err)
			}
		}
	}
}

// dispatchOnce claims a batch of ops (tx#1), then processes each independently with NO tx
// held across the HTTP call. Per-op failures are recorded via fail_outbound_op, not fatal.
func (d *OutboundDispatcher) dispatchOnce(ctx context.Context) error {
	batch := d.Batch
	if batch <= 0 {
		batch = 20
	}
	var ops []claimedOp
	if err := d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT op_id, op_type, connector_id, ticket_id, message_id, ticket_external_id, ticket_subject, body, attempts
			   FROM claim_outbound_ops($1)`, batch)
		if err != nil {
			return fmt.Errorf("claim_outbound_ops: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var o claimedOp
			if err := rows.Scan(&o.ID, &o.Type, &o.ConnectorID, &o.TicketID, &o.MessageID,
				&o.TicketExtID, &o.TicketSubject, &o.Body, &o.Attempts); err != nil {
				return fmt.Errorf("scan claimed op: %w", err)
			}
			ops = append(ops, o)
		}
		return rows.Err()
	}); err != nil {
		return err
	}

	for _, o := range ops {
		if err := d.dispatchOp(ctx, o); err != nil {
			// Attempts is the post-increment count for THIS attempt; once it reaches the cap
			// the op is terminally failed instead of re-queued (else a permanently-failing op
			// would loop forever).
			terminal := o.Attempts >= maxOutboundAttempts
			d.recordFailure(ctx, o.ID, err, terminal)
			d.logger().WarnContext(ctx, "connectors/outbound: op failed",
				"op_id", o.ID, "type", o.Type, "attempts", o.Attempts, "terminal", terminal, "err", err)
		}
	}
	return nil
}

// dispatchOp processes one op: load context (tx) -> unseal + build client (no tx) ->
// HTTP (no tx) -> write-back (tx). Returns an error to trigger fail_outbound_op.
func (d *OutboundDispatcher) dispatchOp(ctx context.Context, o claimedOp) error {
	// Step 1: principal-less context lookup (sealed cred + config). Short tx.
	var (
		bizID, tenant          uuid.UUID
		ctype, baseURL, sealed string
		allowPriv              bool
		configRaw              []byte
	)
	if err := d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT business_id, tenant_root_id, ctype, base_url, allow_private_base_url, sealed_secret, config
			   FROM connector_outbound_context($1)`, o.ConnectorID).
			Scan(&bizID, &tenant, &ctype, &baseURL, &allowPriv, &sealed, &configRaw)
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("connector %s not found or disabled", o.ConnectorID)
		}
		return fmt.Errorf("connector_outbound_context: %w", err)
	}

	// Step 2: unseal credential. Never log sealed or plain.
	plain, err := d.Sealer.Open(sealed)
	if err != nil {
		d.logger().ErrorContext(ctx, "connectors/outbound: unseal failed", "connector_id", o.ConnectorID)
		return fmt.Errorf("unseal connector %s: %w", o.ConnectorID, err)
	}
	var cred Credential
	if err := json.Unmarshal(plain, &cred); err != nil {
		return fmt.Errorf("credential unmarshal: %w", err)
	}
	cfg := map[string]any{}
	if len(configRaw) > 0 {
		// A malformed config must surface as its own error, not silently leave cfg empty and
		// then mis-report as "config missing project_key/issue_type" on the create path.
		if err := json.Unmarshal(configRaw, &cfg); err != nil {
			return fmt.Errorf("connector %s config unmarshal: %w", o.ConnectorID, err)
		}
	}

	// Step 3: build the live, SSRF-safe client (no tx).
	conn, err := d.Registry.BuildSystem(ResolvedConnector{
		ID: o.ConnectorID.String(), Type: ctype, BaseURL: baseURL,
		AllowPrivateBaseURL: allowPriv, Credential: cred, Config: cfg,
	})
	if err != nil {
		return fmt.Errorf("BuildSystem: %w", err)
	}

	switch o.Type {
	case "comment":
		return d.dispatchComment(ctx, conn, o)
	case "create_issue":
		return d.dispatchCreate(ctx, conn, o, cfg)
	default:
		return fmt.Errorf("unknown op type %q", o.Type)
	}
}

// dispatchComment posts a native reply as an external comment, then stamps the comment's
// external_id back onto the native message. Idempotent: if the message already carries an
// external_id (a prior at-least-once attempt succeeded) it does NOT re-POST.
func (d *OutboundDispatcher) dispatchComment(ctx context.Context, conn TicketingConnector, o claimedOp) error {
	if !o.MessageID.Valid {
		return fmt.Errorf("comment op %s missing message_id", o.ID)
	}
	if o.TicketExtID == nil || *o.TicketExtID == "" {
		return fmt.Errorf("comment op %s ticket has no external_id", o.ID)
	}
	msgID := uuid.UUID(o.MessageID.Bytes)

	// Idempotency: if the message already carries an external_id a prior attempt posted it.
	// Re-run the complete DEFINER (no-op on the message, but it marks the op done + audits)
	// WITHOUT re-POSTing.
	//
	// Residual at-least-once dup window (NOT closed): this guard only catches the case where
	// a prior attempt's write-back tx COMMITTED. If PostComment succeeds at Jira but the
	// write-back tx then fails (crash/conn drop before commit), the message external_id stays
	// NULL, so on the next claim this guard sees empty and we re-POST — creating a SECOND Jira
	// comment. Each POST yields a distinct external_id, so the ticket_message unique index does
	// NOT dedup it. Jira exposes no idempotency key, so this window is left open by design (see
	// the plan's documented "crash-after-POST dup window").
	existing, err := d.messageExternalID(ctx, msgID)
	if err != nil {
		return err
	}
	if existing != "" {
		return d.completeComment(ctx, o.ID, msgID, o.ConnectorID, existing)
	}

	body := ""
	if o.Body != nil {
		body = *o.Body
	}
	// HTTP — NO tx held.
	cm, err := conn.PostComment(ctx, *o.TicketExtID, body)
	if err != nil {
		return fmt.Errorf("PostComment: %w", err)
	}
	if cm.ExternalID == "" {
		return fmt.Errorf("comment op %s: upstream returned empty comment id", o.ID)
	}
	// Write-back — short tx.
	return d.completeComment(ctx, o.ID, msgID, o.ConnectorID, cm.ExternalID)
}

// dispatchCreate creates a new external issue from an escalated native ticket, then links
// the ticket to the connector + external id. The dedicated test arrives in T5; the switch
// must handle the branch here.
func (d *OutboundDispatcher) dispatchCreate(ctx context.Context, conn TicketingConnector, o claimedOp, cfg map[string]any) error {
	projectKey, _ := cfg["project_key"].(string)
	issueType, _ := cfg["issue_type"].(string)
	if projectKey == "" || issueType == "" {
		return fmt.Errorf("create op %s: connector config missing project_key/issue_type", o.ID)
	}
	desc := ""
	if o.Body != nil {
		desc = *o.Body
	}
	// HTTP — NO tx held.
	iss, err := conn.CreateIssue(ctx, ExternalIssueDraft{
		ProjectKey: projectKey, IssueType: issueType, Summary: o.TicketSubject, Description: desc,
	})
	if err != nil {
		return fmt.Errorf("CreateIssue: %w", err)
	}
	if iss.ExternalID == "" {
		return fmt.Errorf("create op %s: upstream returned empty issue id", o.ID)
	}
	// Write-back — short tx.
	return d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT complete_outbound_create($1,$2,$3,$4,$5)`,
			o.ID, o.TicketID, o.ConnectorID, iss.ExternalID, iss.URL)
		return e
	})
}

// completeComment stamps the message external_id + marks the op done + audits, in one short
// tx via the migration-0045 DEFINER (idempotent on the message: the DEFINER only writes
// external_id where it was NULL, but always marks the op done).
func (d *OutboundDispatcher) completeComment(ctx context.Context, opID, msgID, connID uuid.UUID, externalID string) error {
	return d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT complete_outbound_comment($1,$2,$3,$4)`,
			opID, msgID, connID, externalID)
		return e
	})
}

// messageExternalID reads a connector-linked message's external_id via the principal-less
// DEFINER (ticket_message is RLS-protected). Empty string = not yet posted.
func (d *OutboundDispatcher) messageExternalID(ctx context.Context, msgID uuid.UUID) (string, error) {
	var ext *string
	if err := d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT message_external_id($1)`, msgID).Scan(&ext)
	}); err != nil {
		return "", fmt.Errorf("message_external_id: %w", err)
	}
	if ext == nil {
		return "", nil
	}
	return *ext, nil
}

// recordFailure marks the op for retry or terminal failure via fail_outbound_op: on a
// non-terminal failure the op is requeued to 'pending' (reclaimable by a later
// claim_outbound_ops pass, which only selects status='pending'); on a terminal failure it
// is set to 'failed'. Best-effort: if THIS fail-tx itself fails it is only logged, NOT
// propagated — the op is then stranded in 'in_progress' and will NOT be reclaimed (claim
// never selects 'in_progress'). There is no reaper for stuck 'in_progress' ops yet; that
// is tracked as follow-up manyforge-a7j.4.9.
func (d *OutboundDispatcher) recordFailure(ctx context.Context, opID uuid.UUID, cause error, terminal bool) {
	if err := d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT fail_outbound_op($1,$2,$3)`, opID, cause.Error(), terminal)
		return e
	}); err != nil {
		d.logger().ErrorContext(ctx, "connectors/outbound: fail_outbound_op failed", "op_id", opID, "err", err)
	}
}

func (d *OutboundDispatcher) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}
