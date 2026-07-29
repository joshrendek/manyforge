//go:build integration

package tenancy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/observability"
	"github.com/manyforge/manyforge/internal/tenancy"
)

func TestTenantMergeReleaseGateMovesEveryModuleAndPreservesState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	metrics := observability.NewMetrics()
	preflightBaseline := metrics.Get(
		observability.MetricTenantMergePreflightTotal,
	)
	successBaseline := metrics.Get(
		observability.MetricTenantMergeSucceeded,
	)
	svc := &tenancy.Service{DB: tdb.App, Metrics: metrics}

	actor, sourceRoot := seedFounder(
		ctx, t, tdb, "release-gate-source-owner@x.test",
	)
	_, destinationRoot := seedFounder(
		ctx, t, tdb, "release-gate-destination-owner@x.test",
	)
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)
	destinationParent, err := svc.CreateSubBusiness(
		ctx, actor, destinationRoot, "Release gate destination parent",
	)
	if err != nil {
		t.Fatalf("create destination parent: %v", err)
	}
	sourceChild, err := svc.CreateSubBusiness(
		ctx, actor, sourceRoot, "Release gate source child",
	)
	if err != nil {
		t.Fatalf("create source child: %v", err)
	}
	viewer := seedMemberAt(
		ctx, t, tdb, sourceRoot, sourceRoot,
		presetRole(ctx, t, tdb, "viewer"),
		"release-gate-source-viewer@x.test",
	)

	ids := map[string]uuid.UUID{
		"role":                    uuid.New(),
		"principal":               uuid.New(),
		"requester":               uuid.New(),
		"ticket":                  uuid.New(),
		"ticket_message":          uuid.New(),
		"company":                 uuid.New(),
		"secret":                  uuid.New(),
		"connector":               uuid.New(),
		"connector_outbound_op":   uuid.New(),
		"agent":                   uuid.New(),
		"agent_run":               uuid.New(),
		"mcp_server":              uuid.New(),
		"repo_connector":          uuid.New(),
		"code_review":             uuid.New(),
		"github_app_installation": uuid.New(),
		"feedback_board":          uuid.New(),
		"telemetry_client":        uuid.New(),
		"notification":            uuid.New(),
		"audit_entry":             uuid.New(),
		"outbox":                  uuid.New(),
	}
	agentRuntimeRole := presetRole(ctx, t, tdb, "agent_runtime")
	messageCreatedAt := time.Now().UTC().Truncate(time.Microsecond)

	seedStatements := []struct {
		name string
		sql  string
		args []any
	}{
		{"custom role", `
			INSERT INTO role (id, tenant_root_id, key, name)
			VALUES ($1, $2, 'release-gate-role', 'Release gate role')`,
			[]any{ids["role"], sourceRoot}},
		{"agent principal", `
			INSERT INTO principal (
			    id, kind, home_business_id, tenant_root_id
			) VALUES ($1, 'agent', $2, $3)`,
			[]any{ids["principal"], sourceChild.ID, sourceRoot}},
		{"agent membership", `
			INSERT INTO membership (
			    principal_id, business_id, tenant_root_id, role_id
			) VALUES ($1, $2, $3, $4)`,
			[]any{
				ids["principal"], sourceChild.ID, sourceRoot,
				agentRuntimeRole,
			}},
		{"requester", `
			INSERT INTO requester (
			    id, business_id, tenant_root_id, email, display_name
			) VALUES (
			    $1, $2, $3, 'release-gate-requester@x.test',
			    'Release Gate Requester'
			)`,
			[]any{ids["requester"], sourceChild.ID, sourceRoot}},
		{"ticket", `
			INSERT INTO ticket (
			    id, business_id, tenant_root_id, requester_id, subject,
			    reply_token, external_id, external_url
			) VALUES (
			    $1, $2, $3, $4, 'Release gate history',
			    'release-gate-reply-token', 'support-ext-42',
			    'https://support.example.test/tickets/42'
			)`,
			[]any{
				ids["ticket"], sourceChild.ID, sourceRoot,
				ids["requester"],
			}},
		{"ticket history", `
			INSERT INTO ticket_message (
			    id, ticket_id, business_id, tenant_root_id, direction,
			    message_id, body_text, created_at
			) VALUES (
			    $1, $2, $3, $4, 'inbound',
			    '<release-gate-history@x.test>', 'preserved history', $5
			)`,
			[]any{
				ids["ticket_message"], ids["ticket"], sourceChild.ID,
				sourceRoot, messageCreatedAt,
			}},
		{"CRM company", `
			INSERT INTO company (id, tenant_root_id, name, domain)
			VALUES ($1, $2, 'Release Gate Company', 'release-gate.example')`,
			[]any{ids["company"], sourceRoot}},
		{"connector secret", `
			INSERT INTO secret (
			    id, business_id, tenant_root_id, scope, sealed_value
			) VALUES ($1, $2, $3, 'connector', 'sealed-release-gate')`,
			[]any{ids["secret"], sourceChild.ID, sourceRoot}},
		{"connector", `
			INSERT INTO connector (
			    id, business_id, tenant_root_id, type, display_name,
			    base_url, secret_ref
			) VALUES (
			    $1, $2, $3, 'jira', 'Release Gate Jira',
			    'https://jira.example.test', $4
			)`,
			[]any{
				ids["connector"], sourceChild.ID, sourceRoot,
				ids["secret"],
			}},
		{"pending connector work", `
			INSERT INTO connector_outbound_op (
			    id, business_id, tenant_root_id, connector_id, ticket_id,
			    op_type, status, body
			) VALUES (
			    $1, $2, $3, $4, $5, 'comment', 'pending',
			    'preserved pending connector work'
			)`,
			[]any{
				ids["connector_outbound_op"], sourceChild.ID, sourceRoot,
				ids["connector"], ids["ticket"],
			}},
		{"agent", `
			INSERT INTO agent (
			    id, business_id, tenant_root_id, principal_id, name,
			    provider, model
			) VALUES (
			    $1, $2, $3, $4, 'Release Gate Agent',
			    'ollama', 'release-gate-model'
			)`,
			[]any{
				ids["agent"], sourceChild.ID, sourceRoot,
				ids["principal"],
			}},
		{"queued agent run", `
			INSERT INTO agent_run (
			    id, agent_id, business_id, tenant_root_id, trigger,
			    status, correlation_id
			) VALUES (
			    $1, $2, $3, $4, 'manual', 'queued',
			    'release-gate-agent-run'
			)`,
			[]any{
				ids["agent_run"], ids["agent"], sourceChild.ID,
				sourceRoot,
			}},
		{"MCP server", `
			INSERT INTO mcp_server (
			    id, business_id, tenant_root_id, name, url
			) VALUES (
			    $1, $2, $3, 'Release Gate MCP',
			    'https://mcp.example.test'
			)`,
			[]any{ids["mcp_server"], sourceChild.ID, sourceRoot}},
		{"repository", `
			INSERT INTO repo_connector (
			    id, business_id, tenant_root_id, display_name, base_url,
			    repo, secret_ref
			) VALUES (
			    $1, $2, $3, 'Release Gate Repo',
			    'https://github.example.test', 'acme/release-gate', $4
			)`,
			[]any{
				ids["repo_connector"], sourceChild.ID, sourceRoot,
				ids["secret"],
			}},
		{"pending code review", `
			INSERT INTO code_review (
			    id, business_id, tenant_root_id, agent_run_id,
			    repo_connector_id, pr_number, head_sha, status,
			    principal_id, agent_id
			) VALUES (
			    $1, $2, $3, $4, $5, 42, 'release-gate-sha',
			    'pending', $6, $7
			)`,
			[]any{
				ids["code_review"], sourceChild.ID, sourceRoot,
				ids["agent_run"], ids["repo_connector"],
				ids["principal"], ids["agent"],
			}},
		{"GitHub installation", `
			INSERT INTO github_app_installation (
			    id, installation_id, account_login, business_id,
			    tenant_root_id, agent_id
			) VALUES (
			    $1, 424242, 'release-gate-org', $2, $3, $4
			)`,
			[]any{
				ids["github_app_installation"], sourceChild.ID,
				sourceRoot, ids["agent"],
			}},
		{"feedback board", `
			INSERT INTO feedback_board (
			    id, business_id, tenant_root_id, slug, name, is_public
			) VALUES (
			    $1, $2, $3, 'release-gate-board',
			    'Release Gate Board', true
			)`,
			[]any{ids["feedback_board"], sourceChild.ID, sourceRoot}},
		{"telemetry client", `
			INSERT INTO telemetry_client (
			    id, business_id, tenant_root_id, kind, name,
			    publishable_key
			) VALUES (
			    $1, $2, $3, 'analytics', 'Release Gate Analytics',
			    'mf_release_gate_public_key'
			)`,
			[]any{ids["telemetry_client"], sourceChild.ID, sourceRoot}},
		{"notification", `
			INSERT INTO notification (
			    id, tenant_root_id, principal_id, kind, ref
			) VALUES (
			    $1, $2, $3, 'release_gate',
			    '{"stable_ref":"release-gate"}'::jsonb
			)`,
			[]any{ids["notification"], sourceRoot, viewer}},
		{"audit history", `
			INSERT INTO audit_entry (
			    id, business_id, tenant_root_id, actor_principal_id,
			    action, target_type, target_id, correlation_id, new_value
			) VALUES (
			    $1, $2, $3, $4, 'release.gate.seeded', 'ticket', $5,
			    'release-gate-history',
			    '{"stable_history":"preserved"}'::jsonb
			)`,
			[]any{
				ids["audit_entry"], sourceChild.ID, sourceRoot, actor,
				ids["ticket"],
			}},
		{"root-bearing queue payload", `
			INSERT INTO outbox (id, tenant_root_id, topic, payload)
			VALUES (
			    $1, $2, 'agent.action.approved',
			    jsonb_build_object(
			        'tenant_root_id', $2::uuid::text,
			        'stable_external_ref', 'queue-release-gate'
			    )
			)`,
			[]any{ids["outbox"], sourceRoot}},
	}
	for _, statement := range seedStatements {
		if _, err := tdb.Super.Exec(
			ctx, statement.sql, statement.args...,
		); err != nil {
			t.Fatalf("seed %s: %v", statement.name, err)
		}
	}

	operation, err := svc.CreateTenantMergeOperation(
		ctx, actor, sourceRoot, destinationParent.ID,
		"release-gate-every-module",
	)
	if err != nil {
		t.Fatalf("create release-gate operation: %v", err)
	}
	ready, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
	if err != nil || ready.Status != "ready" {
		t.Fatalf("release-gate preflight: status=%q err=%v conflicts=%+v",
			ready.Status, err, ready.Conflicts)
	}
	if delta := metrics.Get(
		observability.MetricTenantMergePreflightTotal,
	) - preflightBaseline; delta != 1 {
		t.Errorf("preflight telemetry delta = %d, want 1", delta)
	}
	wantModules := []string{
		"agents", "ai_mcp", "audit", "connectors", "crm", "feedback",
		"github_app", "iam", "identity", "notifications",
		"platform_events", "repositories", "support", "telemetry",
		"tenancy",
	}
	for _, module := range wantModules {
		if count, ok := ready.ModuleCounts[module]; !ok || count.Rows == 0 {
			t.Errorf("preflight module %q count = %+v, want non-zero",
				module, count)
		}
	}
	authorizeTenantMergeCutover(ctx, t, tdb, operation.ID)
	fenced, err := svc.BeginTenantMergeFence(ctx, actor, operation.ID)
	if err != nil || fenced.Status != "running" {
		t.Fatalf("begin release-gate fence: status=%q err=%v",
			fenced.Status, err)
	}

	type writeProbe struct {
		name string
		sql  string
		id   uuid.UUID
	}
	writeProbes := []writeProbe{
		{
			name: "telemetry ingest",
			sql:  "UPDATE telemetry_client SET name=name WHERE id=$1",
			id:   ids["telemetry_client"],
		},
		{
			name: "connector inbound",
			sql:  "UPDATE connector SET updated_at=updated_at WHERE id=$1",
			id:   ids["connector"],
		},
		{
			name: "connector outbound",
			sql: "UPDATE connector_outbound_op " +
				"SET updated_at=updated_at WHERE id=$1",
			id: ids["connector_outbound_op"],
		},
		{
			name: "agent worker",
			sql:  "UPDATE agent_run SET updated_at=updated_at WHERE id=$1",
			id:   ids["agent_run"],
		},
		{
			name: "review worker",
			sql:  "UPDATE code_review SET updated_at=updated_at WHERE id=$1",
			id:   ids["code_review"],
		},
		{
			name: "support mail",
			sql: "UPDATE ticket_message " +
				"SET body_text=body_text WHERE id=$1",
			id: ids["ticket_message"],
		},
		{
			name: "feedback ingest",
			sql:  "UPDATE feedback_board SET name=name WHERE id=$1",
			id:   ids["feedback_board"],
		},
		{
			name: "outbox dispatch",
			sql:  "UPDATE outbox SET attempts=attempts WHERE id=$1",
			id:   ids["outbox"],
		},
	}
	type writeProbeResult struct {
		name string
		err  error
	}
	writeResults := make(chan writeProbeResult, len(writeProbes))
	for _, probe := range writeProbes {
		probe := probe
		go func() {
			_, probeErr := tdb.Super.Exec(ctx, probe.sql, probe.id)
			writeResults <- writeProbeResult{
				name: probe.name,
				err:  probeErr,
			}
		}()
	}
	for range writeProbes {
		result := <-writeResults
		var pgErr *pgconn.PgError
		if !errors.As(result.err, &pgErr) || pgErr.Code != "TM503" {
			t.Errorf("%s concurrent fenced write = %v, want SQLSTATE TM503",
				result.name, result.err)
		}
	}

	type claimProbe struct {
		name string
		sql  string
		id   uuid.UUID
	}
	claimProbes := []claimProbe{
		{
			name: "connector reconcile",
			sql: `SELECT count(*)
			      FROM list_connectors_due_for_reconcile(interval '0')
			      WHERE id=$1`,
			id: ids["connector"],
		},
		{
			name: "connector outbound claim",
			sql: `SELECT count(*)
			      FROM claim_outbound_ops(100, interval '5 minutes')
			      WHERE op_id=$1`,
			id: ids["connector_outbound_op"],
		},
		{
			name: "agent claim",
			sql: `SELECT count(*)
			      FROM claim_next_queued_agent_run()
			      WHERE run_id=$1`,
			id: ids["agent_run"],
		},
		{
			name: "review claim",
			sql: `SELECT count(*)
			      FROM claim_code_reviews(300, 100)
			      WHERE id=$1`,
			id: ids["code_review"],
		},
		{
			name: "outbox claim",
			sql: `SELECT count(*)
			      FROM claim_outbox_batch(1000)
			      WHERE id=$1`,
			id: ids["outbox"],
		},
	}
	type claimProbeResult struct {
		name  string
		count int
		err   error
	}
	claimResults := make(chan claimProbeResult, len(claimProbes))
	for _, probe := range claimProbes {
		probe := probe
		go func() {
			var count int
			probeErr := tdb.Super.QueryRow(
				ctx, probe.sql, probe.id,
			).Scan(&count)
			claimResults <- claimProbeResult{
				name: probe.name, count: count, err: probeErr,
			}
		}()
	}
	for range claimProbes {
		result := <-claimResults
		if result.err != nil || result.count != 0 {
			t.Errorf("%s concurrent fenced claim = count %d err %v, want 0/nil",
				result.name, result.count, result.err)
		}
	}

	for name, query := range map[string]string{
		"analytics daily rollup": `
			SELECT rollup_analytics_daily(
			    interval '0', interval '0'
			)`,
		"analytics page rollup": `
			SELECT rollup_analytics_pageviews(
			    interval '0', interval '0'
			)`,
		"analytics dimension rollup": `
			SELECT rollup_analytics_dimensions(
			    interval '0', interval '0'
			)`,
		"create partitions": "SELECT create_due_partitions()",
		"drop partitions":   "SELECT drop_expired_partitions()",
	} {
		var changed int
		if err := tdb.Super.QueryRow(ctx, query).Scan(&changed); err != nil ||
			changed != 0 {
			t.Errorf("%s while fenced = %d err=%v, want 0/nil",
				name, changed, err)
		}
	}

	succeeded, err := svc.CutoverTenantMerge(ctx, actor, operation.ID)
	if err != nil || succeeded.Status != "succeeded" ||
		succeeded.Manifest == nil {
		t.Fatalf("release-gate cutover: status=%q manifest=%+v err=%v events=%+v",
			succeeded.Status, succeeded.Manifest, err, succeeded.Events)
	}
	if delta := metrics.Get(
		observability.MetricTenantMergeSucceeded,
	) - successBaseline; delta != 1 {
		t.Errorf("success telemetry delta = %d, want 1", delta)
	}

	for table, id := range ids {
		var root uuid.UUID
		if err := tdb.Super.QueryRow(
			ctx,
			"SELECT tenant_root_id FROM "+
				pgxIdentifier(table)+" WHERE id=$1",
			id,
		).Scan(&root); err != nil {
			t.Errorf("read moved %s %s: %v", table, id, err)
		} else if root != destinationRoot {
			t.Errorf("%s %s root = %s, want %s",
				table, id, root, destinationRoot)
		}
	}

	var body, messageID string
	var historyCreatedAt time.Time
	if err := tdb.Super.QueryRow(ctx, `
		SELECT body_text, message_id, created_at
		FROM ticket_message WHERE id=$1`,
		ids["ticket_message"],
	).Scan(&body, &messageID, &historyCreatedAt); err != nil {
		t.Fatalf("read preserved support history: %v", err)
	}
	if body != "preserved history" ||
		messageID != "<release-gate-history@x.test>" ||
		!historyCreatedAt.Equal(messageCreatedAt) {
		t.Errorf("support history changed: body=%q message_id=%q created_at=%s",
			body, messageID, historyCreatedAt)
	}

	var installationID int64
	var repo, publishableKey, boardSlug string
	if err := tdb.Super.QueryRow(ctx, `
		SELECT installation.installation_id,
		       repository.repo,
		       telemetry.publishable_key,
		       board.slug
		FROM github_app_installation installation
		JOIN repo_connector repository ON repository.id=$2
		JOIN telemetry_client telemetry ON telemetry.id=$3
		JOIN feedback_board board ON board.id=$4
		WHERE installation.id=$1`,
		ids["github_app_installation"], ids["repo_connector"],
		ids["telemetry_client"], ids["feedback_board"],
	).Scan(
		&installationID, &repo, &publishableKey, &boardSlug,
	); err != nil {
		t.Fatalf("read stable external references: %v", err)
	}
	if installationID != 424242 || repo != "acme/release-gate" ||
		publishableKey != "mf_release_gate_public_key" ||
		boardSlug != "release-gate-board" {
		t.Errorf("external references changed: installation=%d repo=%q key=%q board=%q",
			installationID, repo, publishableKey, boardSlug)
	}

	var agentRunStatus, reviewStatus, connectorStatus string
	if err := tdb.Super.QueryRow(ctx, `
		SELECT run.status, review.status, outbound.status::text
		FROM agent_run run
		JOIN code_review review ON review.id=$2
		JOIN connector_outbound_op outbound ON outbound.id=$3
		WHERE run.id=$1`,
		ids["agent_run"], ids["code_review"],
		ids["connector_outbound_op"],
	).Scan(
		&agentRunStatus, &reviewStatus, &connectorStatus,
	); err != nil {
		t.Fatalf("read preserved queue states: %v", err)
	}
	if agentRunStatus != "queued" || reviewStatus != "pending" ||
		connectorStatus != "pending" {
		t.Errorf("queue states changed: agent=%q review=%q connector=%q",
			agentRunStatus, reviewStatus, connectorStatus)
	}

	var payloadRaw []byte
	if err := tdb.Super.QueryRow(ctx,
		"SELECT payload FROM outbox WHERE id=$1", ids["outbox"],
	).Scan(&payloadRaw); err != nil {
		t.Fatalf("read rewritten queue payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		t.Fatalf("decode rewritten queue payload: %v", err)
	}
	if payload["tenant_root_id"] != destinationRoot.String() ||
		payload["stable_external_ref"] != "queue-release-gate" {
		t.Errorf("queue payload rewrite = %+v", payload)
	}

	visible, err := svc.ListBusinesses(ctx, viewer)
	if err != nil {
		t.Fatalf("list source viewer businesses after merge: %v", err)
	}
	visibleIDs := make(map[uuid.UUID]bool)
	for _, business := range visible {
		visibleIDs[business.ID] = true
	}
	if !visibleIDs[sourceRoot] || !visibleIDs[sourceChild.ID] ||
		visibleIDs[destinationParent.ID] || visibleIDs[destinationRoot] {
		t.Errorf("source viewer visibility broadened after merge: %v", visibleIDs)
	}

	var verificationRaw []byte
	if err := tdb.Super.QueryRow(ctx,
		"SELECT tenant_merge_verify($1)", operation.ID,
	).Scan(&verificationRaw); err != nil {
		t.Fatalf("run full post-merge verification: %v", err)
	}
	var verification struct {
		OK     bool            `json:"ok"`
		Checks map[string]bool `json:"checks"`
	}
	if err := json.Unmarshal(verificationRaw, &verification); err != nil {
		t.Fatalf("decode full post-merge verification: %v", err)
	}
	if !verification.OK {
		t.Errorf("full post-merge verifier failed: %+v", verification.Checks)
	}
}

func pgxIdentifier(table string) string {
	for _, allowed := range []string{
		"agent", "agent_run", "audit_entry", "code_review", "company",
		"connector", "connector_outbound_op", "feedback_board",
		"github_app_installation", "mcp_server", "notification", "outbox",
		"principal", "repo_connector", "requester", "role", "secret",
		"telemetry_client", "ticket", "ticket_message",
	} {
		if table == allowed {
			return `"` + table + `"`
		}
	}
	panic(fmt.Sprintf("unreviewed tenant-merge fixture table %q", table))
}
