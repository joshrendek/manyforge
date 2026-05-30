//go:build integration

package tenancy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// seedMemberAt creates a verified account + human principal and grants it the
// given role at business (tenant_root scopes the membership). Returns the principal.
func seedMemberAt(ctx context.Context, t *testing.T, tdb *testdb.TestDB, business, tenantRoot, role uuid.UUID, email string) uuid.UUID {
	t.Helper()
	account := uuid.New()
	principal := uuid.New()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'M','active',now(),now(),now())`, []any{account, email}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{principal, account}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$3,$4,now())`, []any{principal, business, tenantRoot, role}},
	}
	for _, s := range stmts {
		if _, err := tdb.Super.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed member: %v", err)
		}
	}
	return principal
}

func presetRole(ctx context.Context, t *testing.T, tdb *testdb.TestDB, key string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key=$1", key).Scan(&id); err != nil {
		t.Fatalf("preset role %q: %v", key, err)
	}
	return id
}

// customRole inserts a tenant-scoped custom role granting perms. Returns its id.
func customRole(ctx context.Context, t *testing.T, tdb *testdb.TestDB, tenantRoot uuid.UUID, name string, perms ...string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	key := "custom-" + id.String()[:8]
	if _, err := tdb.Super.Exec(ctx, `INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,$3,$4,false,now())`, id, tenantRoot, key, name); err != nil {
		t.Fatalf("custom role: %v", err)
	}
	for _, p := range perms {
		if _, err := tdb.Super.Exec(ctx, `INSERT INTO role_permission (role_id,permission_key) VALUES ($1,$2)`, id, p); err != nil {
			t.Fatalf("custom role perm: %v", err)
		}
	}
	return id
}

func memberRole(ctx context.Context, t *testing.T, tdb *testdb.TestDB, principal, business uuid.UUID) uuid.UUID {
	t.Helper()
	var rid uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT role_id FROM membership WHERE principal_id=$1 AND business_id=$2", principal, business).Scan(&rid); err != nil {
		t.Fatalf("member role: %v", err)
	}
	return rid
}

func auditCount(ctx context.Context, t *testing.T, tdb *testdb.TestDB, action string, target uuid.UUID) int {
	t.Helper()
	var n int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM audit_entry WHERE action=$1 AND target_id=$2", action, target).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

func TestChangeMemberRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	adminRole := presetRole(ctx, t, tdb, "admin")
	memberRoleID := presetRole(ctx, t, tdb, "member")
	ownerRole := presetRole(ctx, t, tdb, "owner")

	t.Run("owner reassigns a member, audited", func(t *testing.T) {
		owner, master := seedFounder(ctx, t, tdb, "owner1@x.test")
		bob := seedMemberAt(ctx, t, tdb, master, master, memberRoleID, "bob1@x.test")

		if err := svc.ChangeMemberRole(ctx, owner, master, bob, adminRole); err != nil {
			t.Fatalf("change role: %v", err)
		}
		if got := memberRole(ctx, t, tdb, bob, master); got != adminRole {
			t.Fatalf("role not changed: got %s want %s", got, adminRole)
		}
		if n := auditCount(ctx, t, tdb, "membership.role_changed", bob); n != 1 {
			t.Fatalf("audit entries = %d, want 1", n)
		}
	})

	t.Run("escalation refused: cannot grant a role with perms the actor lacks", func(t *testing.T) {
		owner, master := seedFounder(ctx, t, tdb, "owner2@x.test")
		// admin holds members.manage but NOT business.delete.
		admin := seedMemberAt(ctx, t, tdb, master, master, adminRole, "admin2@x.test")
		bob := seedMemberAt(ctx, t, tdb, master, master, memberRoleID, "bob2@x.test")
		_ = owner
		danger := customRole(ctx, t, tdb, master, "Deleter", "business.delete")

		err := svc.ChangeMemberRole(ctx, admin, master, bob, danger)
		if !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
		if got := memberRole(ctx, t, tdb, bob, master); got != memberRoleID {
			t.Fatalf("role should be unchanged, got %s", got)
		}
	})

	t.Run("assigning Owner is reserved to Owners", func(t *testing.T) {
		_, master := seedFounder(ctx, t, tdb, "owner3@x.test")
		admin := seedMemberAt(ctx, t, tdb, master, master, adminRole, "admin3@x.test")
		bob := seedMemberAt(ctx, t, tdb, master, master, memberRoleID, "bob3@x.test")

		err := svc.ChangeMemberRole(ctx, admin, master, bob, ownerRole)
		if !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("non-owner promoting to Owner: want ErrConflict, got %v", err)
		}
	})

	t.Run("owner may promote to Owner", func(t *testing.T) {
		owner, master := seedFounder(ctx, t, tdb, "owner4@x.test")
		bob := seedMemberAt(ctx, t, tdb, master, master, memberRoleID, "bob4@x.test")

		if err := svc.ChangeMemberRole(ctx, owner, master, bob, ownerRole); err != nil {
			t.Fatalf("owner promote to owner: %v", err)
		}
		if got := memberRole(ctx, t, tdb, bob, master); got != ownerRole {
			t.Fatalf("bob not owner: %s", got)
		}
	})

	t.Run("last Owner cannot be demoted until another exists", func(t *testing.T) {
		owner, master := seedFounder(ctx, t, tdb, "owner5@x.test")

		// Sole owner demoting self is refused.
		err := svc.ChangeMemberRole(ctx, owner, master, owner, adminRole)
		if !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("last-owner demotion: want ErrConflict, got %v", err)
		}
		if got := memberRole(ctx, t, tdb, owner, master); got != ownerRole {
			t.Fatalf("owner should still be owner, got %s", got)
		}

		// With a second owner, the demotion succeeds.
		seedMemberAt(ctx, t, tdb, master, master, ownerRole, "owner5b@x.test")
		if err := svc.ChangeMemberRole(ctx, owner, master, owner, adminRole); err != nil {
			t.Fatalf("demotion with second owner: %v", err)
		}
		if got := memberRole(ctx, t, tdb, owner, master); got != adminRole {
			t.Fatalf("owner not demoted: %s", got)
		}
	})

	t.Run("not-found and conflict guards", func(t *testing.T) {
		owner, master := seedFounder(ctx, t, tdb, "owner6@x.test")
		viewer := seedMemberAt(ctx, t, tdb, master, master, presetRole(ctx, t, tdb, "viewer"), "viewer6@x.test")
		bob := seedMemberAt(ctx, t, tdb, master, master, memberRoleID, "bob6@x.test")

		// Unknown business.
		if err := svc.ChangeMemberRole(ctx, owner, uuid.New(), bob, adminRole); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("unknown business: want ErrNotFound, got %v", err)
		}
		// Target is not a member.
		if err := svc.ChangeMemberRole(ctx, owner, master, uuid.New(), adminRole); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("non-member target: want ErrNotFound, got %v", err)
		}
		// Actor lacks members.manage (viewer) — no oracle, 404.
		if err := svc.ChangeMemberRole(ctx, viewer, master, bob, adminRole); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("viewer actor: want ErrNotFound, got %v", err)
		}
		// Unknown role.
		if err := svc.ChangeMemberRole(ctx, owner, master, bob, uuid.New()); !errors.Is(err, errs.ErrConflict) {
			t.Fatalf("unknown role: want ErrConflict, got %v", err)
		}
	})
}

func TestAccessList(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	adminRole := presetRole(ctx, t, tdb, "admin")
	viewerRole := presetRole(ctx, t, tdb, "viewer")

	owner, master := seedFounder(ctx, t, tdb, "al-owner@x.test")
	sub, err := svc.CreateSubBusiness(ctx, owner, master, "Sub")
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	// An admin granted DIRECTLY at the sub-business (not at any ancestor): under
	// membership_rls they cannot see the owner's ancestor membership, so this
	// exercises the RLS-exempt access_list path.
	subAdmin := seedMemberAt(ctx, t, tdb, sub.ID, master, adminRole, "al-subadmin@x.test")
	viewerP := seedMemberAt(ctx, t, tdb, sub.ID, master, viewerRole, "al-viewer@x.test")

	index := func(ms []tenancy.Member) map[string]tenancy.Member {
		m := map[string]tenancy.Member{}
		for _, x := range ms {
			m[x.PrincipalID] = x
		}
		return m
	}

	t.Run("provenance: direct grant at sub + inherited owner from master", func(t *testing.T) {
		members, err := svc.ListMembers(ctx, subAdmin, sub.ID)
		if err != nil {
			t.Fatalf("list members: %v", err)
		}
		by := index(members)

		og, ok := by[owner.String()]
		if !ok {
			t.Fatalf("owner missing from sub access list (inherited grant not surfaced)")
		}
		if len(og.Grants) != 1 || og.Grants[0].Source != "inherited" || og.Grants[0].SourceBusinessID != master.String() {
			t.Fatalf("owner grant should be inherited from master, got %+v", og.Grants)
		}
		if og.Grants[0].Role.Key != "owner" || !og.Grants[0].Role.Locked {
			t.Errorf("owner grant role: want locked owner, got %+v", og.Grants[0].Role)
		}
		// Owner resolves to the whole catalog (locked role).
		if len(og.EffectivePermissions) < 9 {
			t.Errorf("owner effective_permissions should be the full catalog, got %v", og.EffectivePermissions)
		}
		if og.DisplayName == "" || og.Kind != "human" {
			t.Errorf("owner identity: display_name=%q kind=%q", og.DisplayName, og.Kind)
		}

		sa, ok := by[subAdmin.String()]
		if !ok {
			t.Fatalf("subAdmin missing from access list")
		}
		if len(sa.Grants) != 1 || sa.Grants[0].Source != "direct" || sa.Grants[0].SourceBusinessID != sub.ID.String() {
			t.Fatalf("subAdmin grant should be direct at sub, got %+v", sa.Grants)
		}
	})

	t.Run("gate: viewer (business.read only) cannot view -> 404", func(t *testing.T) {
		if _, err := svc.ListMembers(ctx, viewerP, sub.ID); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("viewer access list: want ErrNotFound, got %v", err)
		}
	})

	t.Run("gate: audit.read holder may view", func(t *testing.T) {
		auditRole := customRole(ctx, t, tdb, master, "Auditor", "audit.read")
		auditor := seedMemberAt(ctx, t, tdb, master, master, auditRole, "al-auditor@x.test")
		members, err := svc.ListMembers(ctx, auditor, master)
		if err != nil {
			t.Fatalf("auditor list members: %v", err)
		}
		if len(index(members)) == 0 {
			t.Fatal("auditor should see the master access list")
		}
	})
}
