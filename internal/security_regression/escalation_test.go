//go:build integration

package security_regression

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/invitations"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// escTenant is a seeded tenant with three human principals at its master: an
// Owner (full catalog), an Admin (the admin preset, which deliberately LACKS
// business.delete and ownership.transfer), and a plain Member. dangerRole is a
// tenant custom role granting business.delete — a permission the Admin does not
// hold — and readRole grants only business.read.
type escTenant struct {
	master                           uuid.UUID
	owner, admin, member             uuid.UUID
	ownerRole, adminRole, memberRole uuid.UUID
	dangerRole, readRole             uuid.UUID
}

func seedEscalationTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) escTenant {
	t.Helper()
	preset := func(key string) uuid.UUID {
		var id uuid.UUID
		if err := tdb.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key=$1", key).Scan(&id); err != nil {
			t.Fatalf("preset role %q: %v", key, err)
		}
		return id
	}
	e := escTenant{
		master: uuid.New(), owner: uuid.New(), admin: uuid.New(), member: uuid.New(),
		ownerRole: preset("owner"), adminRole: preset("admin"), memberRole: preset("member"),
		dangerRole: uuid.New(), readRole: uuid.New(),
	}
	aOwner, aAdmin, aMember := uuid.New(), uuid.New(), uuid.New()
	// Unique emails per seed call so the suite can seed several tenants.
	em := func(p uuid.UUID) string { return "esc-" + p.String() + "@x.test" }

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'O','active',now(),now(),now()),($3,$4,'A','active',now(),now(),now()),($5,$6,'M','active',now(),now(),now())`,
			[]any{aOwner, em(e.owner), aAdmin, em(e.admin), aMember, em(e.member)}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now()),($3,'human',$4,now()),($5,'human',$6,now())`,
			[]any{e.owner, aOwner, e.admin, aAdmin, e.member, aMember}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'EscCo','active',now(),now())`, []any{e.master}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{e.master}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now()),($4,$2,$2,$5,now()),($6,$2,$2,$7,now())`,
			[]any{e.owner, e.master, e.ownerRole, e.admin, e.adminRole, e.member, e.memberRole}},
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'esc-danger','Danger',false,now()),($3,$2,'esc-readonly','ReadOnly',false,now())`,
			[]any{e.dangerRole, e.master, e.readRole}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'business.delete'),($2,'business.read')`, []any{e.dangerRole, e.readRole}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return e
}

func memberRoleID(ctx context.Context, t *testing.T, tdb *testdb.TestDB, principal, business uuid.UUID) uuid.UUID {
	t.Helper()
	var rid uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT role_id FROM membership WHERE principal_id=$1 AND business_id=$2", principal, business).Scan(&rid); err != nil {
		t.Fatalf("read member role: %v", err)
	}
	return rid
}

func rolePermSet(ctx context.Context, t *testing.T, tdb *testdb.TestDB, role uuid.UUID) map[string]bool {
	t.Helper()
	rows, err := tdb.Super.Query(ctx, "SELECT permission_key FROM role_permission WHERE role_id=$1", role)
	if err != nil {
		t.Fatalf("read role perms: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		out[k] = true
	}
	return out
}

// TestEscalationRefused proves FR-023 holds at every place a principal can
// confer authority — assigning a member's role, editing a custom role, and
// inviting — exercised through the service layer as the real RLS-subject
// manyforge_app role. An Admin (who lacks business.delete / ownership.transfer
// and is not an Owner) must not be able to confer any of those via any path; a
// legitimate within-authority action is the control that proves the path works.
// The invite-accept boundary is covered structurally in escalation_pin_test.go:
// accept_invitation materialises the role stored on the (create-time bounded)
// invitation, so acceptance cannot escalate.
func TestEscalationRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	ten := &tenancy.Service{DB: tdb.App}
	rbac := &authz.Service{DB: tdb.App}
	inv := &invitations.Service{DB: tdb.App} // nil Mailer: Create refuses before any send

	t.Run("assign: admin cannot confer perms it lacks or the Owner role", func(t *testing.T) {
		e := seedEscalationTenant(ctx, t, tdb)

		if err := ten.ChangeMemberRole(ctx, e.admin, e.master, e.member, e.dangerRole); !errors.Is(err, errs.ErrConflict) {
			t.Errorf("admin assigns business.delete role: want ErrConflict, got %v", err)
		}
		if err := ten.ChangeMemberRole(ctx, e.admin, e.master, e.member, e.ownerRole); !errors.Is(err, errs.ErrConflict) {
			t.Errorf("admin assigns Owner role: want ErrConflict, got %v", err)
		}
		if got := memberRoleID(ctx, t, tdb, e.member, e.master); got != e.memberRole {
			t.Errorf("member's role must be unchanged after refused escalations, got %s", got)
		}
		// Control: an Owner assigning a role within its authority succeeds.
		if err := ten.ChangeMemberRole(ctx, e.owner, e.master, e.member, e.adminRole); err != nil {
			t.Errorf("control: owner assigns admin role: %v", err)
		}
	})

	t.Run("edit: admin cannot add a permission it lacks to a custom role", func(t *testing.T) {
		e := seedEscalationTenant(ctx, t, tdb)

		escalate := []string{"business.delete"}
		if _, err := rbac.UpdateRole(ctx, e.admin, e.master, e.readRole, nil, &escalate); !errors.Is(err, errs.ErrConflict) {
			t.Errorf("admin adds business.delete to a role: want ErrConflict, got %v", err)
		}
		if perms := rolePermSet(ctx, t, tdb, e.readRole); perms["business.delete"] || !perms["business.read"] {
			t.Errorf("role perms must be unchanged after refused edit, got %v", perms)
		}
		// Control: editing within authority succeeds.
		within := []string{"members.read"}
		if _, err := rbac.UpdateRole(ctx, e.admin, e.master, e.readRole, nil, &within); err != nil {
			t.Errorf("control: admin edits role within authority: %v", err)
		}
	})

	t.Run("invite: admin cannot invite with a role above its own", func(t *testing.T) {
		e := seedEscalationTenant(ctx, t, tdb)

		if err := inv.Create(ctx, e.admin, e.master, e.ownerRole, "esc-inv-owner@x.test"); !errors.Is(err, errs.ErrConflict) {
			t.Errorf("admin invites as Owner: want ErrConflict, got %v", err)
		}
		if err := inv.Create(ctx, e.admin, e.master, e.dangerRole, "esc-inv-danger@x.test"); !errors.Is(err, errs.ErrConflict) {
			t.Errorf("admin invites with business.delete role: want ErrConflict, got %v", err)
		}
		// Control: inviting within authority succeeds.
		if err := inv.Create(ctx, e.admin, e.master, e.memberRole, "esc-inv-member@x.test"); err != nil {
			t.Errorf("control: admin invites as member: %v", err)
		}
	})
}
