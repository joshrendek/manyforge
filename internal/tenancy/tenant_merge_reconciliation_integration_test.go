//go:build integration

package tenancy_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/tenancy"
)

func reconciliationPolicy(
	t *testing.T,
	plan *tenancy.TenantMergeReconciliationPlan,
	key string,
) tenancy.TenantMergeReconciliationPolicy {
	t.Helper()
	if plan == nil {
		t.Fatal("reconciliation plan is nil")
	}
	for _, policy := range plan.Policies {
		if policy.Key == key {
			return policy
		}
	}
	t.Fatalf("reconciliation plan omits policy %q: %+v", key, plan.Policies)
	return tenancy.TenantMergeReconciliationPolicy{}
}

func visibleBusinessIDs(
	ctx context.Context,
	t *testing.T,
	svc *tenancy.Service,
	principal uuid.UUID,
) []string {
	t.Helper()
	businesses, err := svc.ListBusinesses(ctx, principal)
	if err != nil {
		t.Fatalf("list businesses for %s: %v", principal, err)
	}
	ids := make([]string, 0, len(businesses))
	for _, business := range businesses {
		ids = append(ids, business.ID.String())
	}
	sort.Strings(ids)
	return ids
}

func sortedUUIDStrings(ids ...uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	sort.Strings(out)
	return out
}

type tenantMergeSeedStatement struct {
	sql  string
	args []any
}

func execTenantMergeSeed(
	ctx context.Context,
	t *testing.T,
	tdb *testdb.TestDB,
	description string,
	statements ...tenantMergeSeedStatement,
) {
	t.Helper()
	for _, statement := range statements {
		if _, err := tdb.Super.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", description, err)
		}
	}
}

func TestTenantMergeReconciliationPreservesIdentityAndAccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	actor, sourceRoot := seedFounder(
		ctx, t, tdb, "reconciliation-actor@x.test",
	)
	destinationFounder, destinationRoot := seedFounder(
		ctx, t, tdb, "reconciliation-destination@x.test",
	)
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)

	destinationParent, err := svc.CreateSubBusiness(
		ctx, actor, destinationRoot, "Destination parent",
	)
	if err != nil {
		t.Fatalf("create destination parent: %v", err)
	}
	destinationSibling, err := svc.CreateSubBusiness(
		ctx, actor, destinationRoot, "Destination sibling",
	)
	if err != nil {
		t.Fatalf("create destination sibling: %v", err)
	}
	sourceChild, err := svc.CreateSubBusiness(
		ctx, actor, sourceRoot, "Source child",
	)
	if err != nil {
		t.Fatalf("create source child: %v", err)
	}

	ownerRole := presetRole(ctx, t, tdb, "owner")
	memberRole := presetRole(ctx, t, tdb, "member")
	agentRuntimeRole := presetRole(ctx, t, tdb, "agent_runtime")
	sourceOwnerOnly := seedMemberAt(
		ctx, t, tdb, sourceRoot, sourceRoot, ownerRole,
		"reconciliation-source-owner@x.test",
	)
	sourceRole := customRole(
		ctx, t, tdb, sourceRoot, "Source specialist", "business.read",
	)
	crossTenantMember := seedMemberAt(
		ctx, t, tdb, sourceChild.ID, sourceRoot, sourceRole,
		"reconciliation-cross-member@x.test",
	)
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO membership
		    (principal_id, business_id, tenant_root_id, role_id, granted_at)
		VALUES ($1, $2, $3, $4, now())`,
		crossTenantMember, destinationSibling.ID, destinationRoot, memberRole,
	); err != nil {
		t.Fatalf("grant destination sibling membership: %v", err)
	}

	agentPrincipal := uuid.New()
	agentID := uuid.New()
	execTenantMergeSeed(ctx, t, tdb, "seed source agent",
		tenantMergeSeedStatement{sql: `
		INSERT INTO principal
		    (id, kind, home_business_id, tenant_root_id, created_at)
		VALUES ($1, 'agent', $2, $3, now())`,
			args: []any{agentPrincipal, sourceChild.ID, sourceRoot}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO membership
		    (principal_id, business_id, tenant_root_id, role_id, granted_at)
		VALUES ($1, $2, $3, $4, now())`,
			args: []any{
				agentPrincipal, sourceChild.ID, sourceRoot, agentRuntimeRole,
			}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO agent
		    (id, business_id, tenant_root_id, principal_id, name, provider,
		     model, system_prompt, allowed_tools, autonomy_mode, enabled,
		     monthly_budget_cents, created_at, updated_at)
		VALUES
		    ($1, $2, $3, $4, 'Merge agent', 'ollama', 'merge-model', '',
		     '{}', 1, true, 0, now(), now())`,
			args: []any{
				agentID, sourceChild.ID, sourceRoot, agentPrincipal,
			}},
	)

	credentialID := uuid.New()
	secretID := uuid.New()
	connectorID := uuid.New()
	installationID := uuid.New()
	execTenantMergeSeed(ctx, t, tdb, "seed credentials/connectors/installation",
		tenantMergeSeedStatement{sql: `
		INSERT INTO ai_provider_credential
		    (id, business_id, tenant_root_id, provider, sealed_key_ref,
		     default_model, created_at, updated_at)
		VALUES ($1, $2, $3, 'ollama', 'sealed-ai-value', 'merge-model',
		        now(), now())`,
			args: []any{credentialID, sourceChild.ID, sourceRoot}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO secret
		    (id, business_id, tenant_root_id, scope, sealed_value,
		     created_at, updated_at)
		VALUES ($1, $2, $3, 'connector', 'sealed-connector-value',
		        now(), now())`,
			args: []any{
				secretID, sourceChild.ID, sourceRoot,
			}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO connector
		    (id, business_id, tenant_root_id, type, display_name, base_url,
		     secret_ref, config, status, created_at, updated_at)
		VALUES ($1, $2, $3, 'jira', 'Merge Jira',
		        'https://merge-jira.example.test', $4, '{}', 'enabled',
		        now(), now())`,
			args: []any{
				connectorID, sourceChild.ID, sourceRoot, secretID,
			}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO github_app_installation
		    (id, installation_id, account_login, account_type, business_id,
		     tenant_root_id, agent_id, enabled, config, created_at, updated_at)
		VALUES ($1, 900001, 'merge-installation', 'Organization', $2, $3,
		        $4, true, '{}', now(), now())`,
			args: []any{
				installationID, sourceChild.ID, sourceRoot, agentID,
			}},
	)

	invitationID := uuid.New()
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO invitation
		    (id, business_id, tenant_root_id, email, role_id, token_hash,
		     status, created_by, expires_at, created_at)
		VALUES ($1, $2, $3, 'invited-user@x.test', $4, $5, 'pending', $6,
		        now() + interval '1 day', now())`,
		invitationID, sourceChild.ID, sourceRoot, sourceRole,
		"merge-invitation-"+invitationID.String(), actor,
	); err != nil {
		t.Fatalf("seed invitation: %v", err)
	}

	if got, want := visibleBusinessIDs(
		ctx, t, svc, sourceOwnerOnly,
	), sortedUUIDStrings(sourceRoot, sourceChild.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("source owner visibility before merge = %v, want %v", got, want)
	}
	if got, want := visibleBusinessIDs(
		ctx, t, svc, crossTenantMember,
	), sortedUUIDStrings(sourceChild.ID, destinationSibling.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("cross-tenant member visibility before merge = %v, want %v", got, want)
	}

	operation, err := svc.CreateTenantMergeOperation(
		ctx, actor, sourceRoot, destinationParent.ID,
		"reconciliation-preserves-identities",
	)
	if err != nil {
		t.Fatalf("create operation: %v", err)
	}
	ready, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if ready.Status != "ready" || ready.ReconciliationPlan == nil {
		t.Fatalf("preflight status/plan = %q/%+v; conflicts=%+v",
			ready.Status, ready.ReconciliationPlan, ready.Conflicts)
	}
	if policy := reconciliationPolicy(
		t, ready.ReconciliationPlan, "agent_principals",
	); policy.Action != "rewrite_root_preserve_home_business" ||
		policy.Count != 1 || policy.IdentityDigest == "" {
		t.Errorf("agent policy = %+v", policy)
	}
	if policy := reconciliationPolicy(
		t, ready.ReconciliationPlan, "custom_roles",
	); policy.Action != "rewrite_only_if_key_unique" ||
		policy.Count != 1 || policy.PermissionCount != 1 ||
		policy.PermissionDigest == "" {
		t.Errorf("custom-role policy = %+v", policy)
	}
	if policy := reconciliationPolicy(
		t, ready.ReconciliationPlan, "credentials_and_connectors",
	); policy.Action != "rewrite_root_preserve_ids_and_ciphertext" ||
		policy.Count != 3 {
		t.Errorf("credential/connector policy = %+v", policy)
	}
	if policy := reconciliationPolicy(
		t, ready.ReconciliationPlan, "external_installations",
	); policy.Action != "rewrite_linked_scope_preserve_installation_id" ||
		policy.Count != 1 {
		t.Errorf("installation policy = %+v", policy)
	}

	// role_permission is not a tenant-root table. A permission change must still
	// invalidate the exact reconciliation plan.
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO role_permission (role_id, permission_key)
		VALUES ($1, 'tickets.read')`,
		sourceRole,
	); err != nil {
		t.Fatalf("mutate custom-role permissions: %v", err)
	}
	stale, err := svc.ValidateTenantMergePreflight(ctx, actor, operation.ID)
	if err != nil || stale.Current ||
		stale.Operation.Status != "preflight_required" {
		t.Fatalf("role-permission mutation did not stale plan: result=%+v err=%v",
			stale, err)
	}
	if _, err := tdb.Super.Exec(ctx, `
		DELETE FROM role_permission
		WHERE role_id=$1 AND permission_key='tickets.read'`,
		sourceRole,
	); err != nil {
		t.Fatalf("restore custom-role permissions: %v", err)
	}
	ready, err = svc.PreflightTenantMerge(ctx, actor, operation.ID)
	if err != nil || ready.Status != "ready" {
		t.Fatalf("re-preflight after permission restore: status=%q err=%v conflicts=%+v",
			ready.Status, err, ready.Conflicts)
	}

	if _, err := svc.BeginTenantMergeFence(ctx, actor, operation.ID); err != nil {
		t.Fatalf("begin fence: %v", err)
	}
	fencedPermissionErr := tdb.App.WithPrincipal(
		ctx, actor, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO role_permission (role_id, permission_key)
				VALUES ($1, 'tickets.read')`,
				sourceRole,
			)
			return err
		},
	)
	if !errors.Is(fencedPermissionErr, errs.ErrTenantMaintenance) {
		t.Fatalf("fenced role-permission write = %v, want ErrTenantMaintenance",
			fencedPermissionErr)
	}
	if _, err := svc.CancelTenantMergeFence(ctx, actor, operation.ID); err != nil {
		t.Fatalf("cancel fence: %v", err)
	}
	ready, err = svc.PreflightTenantMerge(ctx, actor, operation.ID)
	if err != nil || ready.Status != "ready" {
		t.Fatalf("re-preflight after fence cancel: status=%q err=%v conflicts=%+v",
			ready.Status, err, ready.Conflicts)
	}

	succeeded, err := svc.CutoverTenantMerge(ctx, actor, operation.ID)
	if err != nil {
		t.Fatalf("cutover: %v", err)
	}
	if succeeded.Status != "succeeded" {
		t.Fatalf("cutover status = %q; conflicts=%+v events=%+v",
			succeeded.Status, succeeded.Conflicts, succeeded.Events)
	}
	var appliedPlanHash string
	for _, event := range succeeded.Events {
		if event.Event == "reconciliation.applied" {
			appliedPlanHash, _ = event.Metadata["hash"].(string)
		}
	}
	if succeeded.ReconciliationHash == nil ||
		appliedPlanHash != *succeeded.ReconciliationHash {
		t.Errorf("applied reconciliation event hash = %q, want %v",
			appliedPlanHash, succeeded.ReconciliationHash)
	}

	if got, want := visibleBusinessIDs(
		ctx, t, svc, sourceOwnerOnly,
	), sortedUUIDStrings(sourceRoot, sourceChild.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("former source owner gained lateral access: got %v, want %v", got, want)
	}
	if got, want := visibleBusinessIDs(
		ctx, t, svc, crossTenantMember,
	), sortedUUIDStrings(sourceChild.ID, destinationSibling.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("cross-tenant member access changed laterally: got %v, want %v", got, want)
	}
	if got, want := visibleBusinessIDs(
		ctx, t, svc, destinationFounder,
	), sortedUUIDStrings(
		destinationRoot, destinationParent.ID, destinationSibling.ID,
		sourceRoot, sourceChild.ID,
	); !reflect.DeepEqual(got, want) {
		t.Errorf("destination owner visibility after merge = %v, want %v", got, want)
	}
	if got, want := visibleBusinessIDs(
		ctx, t, svc, actor,
	), sortedUUIDStrings(
		destinationRoot, destinationParent.ID, destinationSibling.ID,
		sourceRoot, sourceChild.ID,
	); !reflect.DeepEqual(got, want) {
		t.Errorf("Owner of both tenants visibility after merge = %v, want %v",
			got, want)
	}

	var movedAgentRoot, movedAgentHome, movedMembershipRoot uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		SELECT p.tenant_root_id, p.home_business_id, m.tenant_root_id
		FROM principal p
		JOIN membership m ON m.principal_id = p.id
		WHERE p.id=$1`,
		agentPrincipal,
	).Scan(&movedAgentRoot, &movedAgentHome, &movedMembershipRoot); err != nil {
		t.Fatalf("read moved agent identity: %v", err)
	}
	if movedAgentRoot != destinationRoot ||
		movedMembershipRoot != destinationRoot ||
		movedAgentHome != sourceChild.ID {
		t.Errorf("moved agent root/home/membership = %s/%s/%s, want %s/%s/%s",
			movedAgentRoot, movedAgentHome, movedMembershipRoot,
			destinationRoot, sourceChild.ID, destinationRoot)
	}

	var roleRoot, invitationRoot uuid.UUID
	var permissionKeys []string
	if err := tdb.Super.QueryRow(ctx, `
		SELECT r.tenant_root_id,
		       array_agg(rp.permission_key ORDER BY rp.permission_key)
		FROM role r
		JOIN role_permission rp ON rp.role_id = r.id
		WHERE r.id=$1
		GROUP BY r.tenant_root_id`,
		sourceRole,
	).Scan(&roleRoot, &permissionKeys); err != nil {
		t.Fatalf("read moved custom role: %v", err)
	}
	if roleRoot != destinationRoot ||
		!reflect.DeepEqual(permissionKeys, []string{"business.read"}) {
		t.Errorf("moved role root/permissions = %s/%v", roleRoot, permissionKeys)
	}
	if err := tdb.Super.QueryRow(ctx,
		"SELECT tenant_root_id FROM invitation WHERE id=$1", invitationID,
	).Scan(&invitationRoot); err != nil {
		t.Fatalf("read moved invitation: %v", err)
	}
	if invitationRoot != destinationRoot {
		t.Errorf("moved invitation root = %s, want %s",
			invitationRoot, destinationRoot)
	}

	var credentialRoot, secretRoot, connectorRoot, installationRoot uuid.UUID
	var sealedAI, sealedConnector string
	var externalInstallationID int64
	if err := tdb.Super.QueryRow(ctx, `
		SELECT credential.tenant_root_id, credential.sealed_key_ref,
		       secret_row.tenant_root_id, secret_row.sealed_value,
		       connector_row.tenant_root_id,
		       installation.tenant_root_id, installation.installation_id
		FROM ai_provider_credential credential
		JOIN secret secret_row ON secret_row.id=$2
		JOIN connector connector_row ON connector_row.id=$3
		JOIN github_app_installation installation ON installation.id=$4
		WHERE credential.id=$1`,
		credentialID, secretID, connectorID, installationID,
	).Scan(
		&credentialRoot, &sealedAI,
		&secretRoot, &sealedConnector,
		&connectorRoot, &installationRoot, &externalInstallationID,
	); err != nil {
		t.Fatalf("read moved credentials/installations: %v", err)
	}
	if credentialRoot != destinationRoot ||
		secretRoot != destinationRoot ||
		connectorRoot != destinationRoot ||
		installationRoot != destinationRoot ||
		sealedAI != "sealed-ai-value" ||
		sealedConnector != "sealed-connector-value" ||
		externalInstallationID != 900001 {
		t.Errorf("credential/install reconciliation changed identity or secret: roots=%s/%s/%s/%s sealed=%q/%q installation=%d",
			credentialRoot, secretRoot, connectorRoot, installationRoot,
			sealedAI, sealedConnector, externalInstallationID)
	}

	// The former source is now an ordinary business. Removing every direct
	// Owner grant at S must not invoke the tenant-root last-Owner invariant;
	// destination ownership remains the root invariant.
	if _, err := tdb.Super.Exec(ctx, `
		DELETE FROM membership
		WHERE business_id=$1 AND role_id=$2`,
		sourceRoot, ownerRole,
	); err != nil {
		t.Fatalf("remove former-source Owner grants: %v", err)
	}
	var formerSourceOwners, destinationOwners int
	if err := tdb.Super.QueryRow(ctx, `
		SELECT
		    (SELECT count(*) FROM membership
		     WHERE business_id=$1 AND role_id=$3),
		    (SELECT count(*) FROM membership
		     WHERE business_id=$2 AND tenant_root_id=$2 AND role_id=$3)`,
		sourceRoot, destinationRoot, ownerRole,
	).Scan(&formerSourceOwners, &destinationOwners); err != nil {
		t.Fatalf("inspect root-owner invariants: %v", err)
	}
	if formerSourceOwners != 0 || destinationOwners < 1 {
		t.Errorf("former/destination owner counts = %d/%d, want 0/>=1",
			formerSourceOwners, destinationOwners)
	}
}

func TestTenantMergeReconciliationBlocksIdentityCollisionsAndMalformedLinks(
	t *testing.T,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	actor, sourceRoot := seedFounder(
		ctx, t, tdb, "reconciliation-conflict-source@x.test",
	)
	_, destinationRoot := seedFounder(
		ctx, t, tdb, "reconciliation-conflict-destination@x.test",
	)
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)

	destinationRole := customRole(
		ctx, t, tdb, destinationRoot, "Destination-only role", "business.read",
	)
	execTenantMergeSeed(ctx, t, tdb, "seed reconciliation conflicts",
		tenantMergeSeedStatement{sql: `
		INSERT INTO company (id, tenant_root_id, name, domain)
		VALUES
		    (gen_random_uuid(), $1, 'Source company', 'collision.example'),
		    (gen_random_uuid(), $2, 'Destination company', 'collision.example')`,
			args: []any{sourceRoot, destinationRoot}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO contact
		    (id, tenant_root_id, primary_email, display_name)
		VALUES
		    (gen_random_uuid(), $1, 'same-contact@x.test', 'Source contact'),
		    (gen_random_uuid(), $2, 'same-contact@x.test', 'Destination contact')`,
			args: []any{sourceRoot, destinationRoot}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO requester
		    (id, business_id, tenant_root_id, email)
		VALUES
		    (gen_random_uuid(), $1, $1, 'same-requester@x.test'),
		    (gen_random_uuid(), $2, $2, 'same-requester@x.test')`,
			args: []any{sourceRoot, destinationRoot}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO email_domain
		    (id, business_id, tenant_root_id, domain, mode, verify_token)
		VALUES
		    (gen_random_uuid(), $1, $1, 'mail-collision.example',
		     'forward_in', 'source-token'),
		    (gen_random_uuid(), $2, $2, 'mail-collision.example',
		     'forward_in', 'destination-token')`,
			args: []any{sourceRoot, destinationRoot}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO inbound_address
		    (id, business_id, tenant_root_id, address, kind)
		VALUES
		    (gen_random_uuid(), $1, $1, 'same-route@inbound.test', 'system'),
		    (gen_random_uuid(), $2, $2, 'same-route@inbound.test', 'system')`,
			args: []any{sourceRoot, destinationRoot}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO invitation
		    (id, business_id, tenant_root_id, email, role_id, token_hash,
		     status, created_by, expires_at)
		VALUES
		    (gen_random_uuid(), $1, $1, 'bad-role-invite@x.test', $2,
		     'bad-role-invite-token', 'pending', $3, now() + interval '1 day')`,
			args: []any{sourceRoot, destinationRole, actor}},
		tenantMergeSeedStatement{sql: `
		INSERT INTO github_app_installation
		    (id, installation_id, account_login, business_id, tenant_root_id,
		     agent_id)
		VALUES
		    (gen_random_uuid(), 900002, 'malformed-installation', $1, $1,
		     gen_random_uuid())`,
			args: []any{sourceRoot}},
	)

	operation, err := svc.CreateTenantMergeOperation(
		ctx, actor, sourceRoot, destinationRoot,
		"reconciliation-identity-conflicts",
	)
	if err != nil {
		t.Fatalf("create operation: %v", err)
	}
	blocked, err := svc.PreflightTenantMerge(ctx, actor, operation.ID)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if blocked.Status != "preflight_required" ||
		blocked.ReconciliationPlan == nil {
		t.Fatalf("blocked preflight status/plan = %q/%+v",
			blocked.Status, blocked.ReconciliationPlan)
	}
	for _, code := range []string{
		"company_domain_collision",
		"contact_email_collision",
		"requester_email_collision",
		"email_domain_collision",
		"inbound_address_collision",
		"invitation_role_scope_invalid",
		"github_installation_scope_invalid",
	} {
		if !hasFinding(blocked.Conflicts, code) {
			t.Errorf("preflight conflicts omit %q: %+v", code, blocked.Conflicts)
		}
		if !hasFinding(blocked.ReconciliationPlan.Conflicts, code) {
			t.Errorf("reconciliation plan conflicts omit %q: %+v",
				code, blocked.ReconciliationPlan.Conflicts)
		}
	}
}
