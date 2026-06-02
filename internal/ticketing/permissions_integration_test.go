//go:build integration

package ticketing

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// T063 (SC-009/FR-016): the six support permissions form an enforcement matrix —
// a role grants EXACTLY the permissions assigned to it and denies the rest, and
// the built-in presets resolve to the data-model.md matrix. Driven through the
// real authz.Resolve under each principal's RLS context (the same path
// httpx.RequirePermission uses), so this is the behavioral proof that the gate
// keys on grants, not on anything else. (Human-vs-agent uniformity — that the
// resolver never branches on principal kind — is pinned fast/Docker-free in
// internal/security_regression/permission_matrix_pin_test.go.)

// supportPerms is the frozen six-permission catalog for spec 002 (migration 0015).
var supportPerms = []string{
	"tickets.read",
	"tickets.reply",
	"tickets.write",
	"tickets.assign",
	"tickets.delete",
	"inbox.manage",
}

// permMatrixTenant is a single master tenant seeded with one principal per preset
// role plus one principal per support permission whose CUSTOM role grants only
// that single permission.
type permMatrixTenant struct {
	master uuid.UUID
	owner  uuid.UUID
	admin  uuid.UUID
	member uuid.UUID
	viewer uuid.UUID
	// single[perm] is a principal whose custom role grants ONLY perm.
	single map[string]uuid.UUID
}

// seedPermMatrixTenant inserts the whole fixture in one Super (RLS-exempt)
// transaction so the deferred last-Owner guard validates at commit (the owner
// membership satisfies it). Preset role ids are read from the catalog via
// presetRole (read_integration_test.go).
func seedPermMatrixTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) permMatrixTenant {
	t.Helper()
	pt := permMatrixTenant{master: uuid.New(), single: map[string]uuid.UUID{}}

	ownerRole := presetRole(ctx, t, tdb, "owner")
	adminRole := presetRole(ctx, t, tdb, "admin")
	memberRole := presetRole(ctx, t, tdb, "member")
	viewerRole := presetRole(ctx, t, tdb, "viewer")

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		 VALUES ($1,NULL,$1,'PermCo','active',now(),now())`, pt.master); err != nil {
		t.Fatalf("seed business: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id)
		 VALUES ($1,$1,0,$1)`, pt.master); err != nil {
		t.Fatalf("seed closure: %v", err)
	}

	// addMember creates account+principal and a membership at the master business
	// under roleID, returning the principal id.
	addMember := func(label string, roleID uuid.UUID) uuid.UUID {
		t.Helper()
		acct, pid := uuid.New(), uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at)
			 VALUES ($1,$2,$3,'active',now(),now(),now())`,
			acct, "pm-"+label+"-"+pid.String()+"@x.test", label); err != nil {
			t.Fatalf("seed account %s: %v", label, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			pid, acct); err != nil {
			t.Fatalf("seed principal %s: %v", label, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at)
			 VALUES ($1,$2,$2,$3,now())`, pid, pt.master, roleID); err != nil {
			t.Fatalf("seed membership %s: %v", label, err)
		}
		return pid
	}

	pt.owner = addMember("owner", ownerRole)
	pt.admin = addMember("admin", adminRole)
	pt.member = addMember("member", memberRole)
	pt.viewer = addMember("viewer", viewerRole)

	// One custom role per support permission, granting exactly that permission,
	// with a dedicated principal as its sole member.
	for _, perm := range supportPerms {
		roleID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at)
			 VALUES ($1,$2,$3,$4,false,now())`,
			roleID, pt.master, "permmatrix-"+perm, "only "+perm); err != nil {
			t.Fatalf("seed role for %s: %v", perm, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,$2)`,
			roleID, perm); err != nil {
			t.Fatalf("seed role_permission for %s: %v", perm, err)
		}
		pt.single[perm] = addMember("only-"+perm, roleID)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return pt
}

// TestSupportPermissionMatrix proves grant-exactly-its-set for each of the six
// permissions and the owner/admin/member/viewer preset matrix.
func TestSupportPermissionMatrix(t *testing.T) {
	ctx, tdb := startReadDB(t)
	pt := seedPermMatrixTenant(ctx, t, tdb)

	resolve := func(pid uuid.UUID) authz.PermissionSet {
		t.Helper()
		var set authz.PermissionSet
		if err := tdb.App.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
			s, err := authz.Resolve(ctx, tx, pid, pt.master)
			if err != nil {
				return err
			}
			set = s
			return nil
		}); err != nil {
			t.Fatalf("resolve %s: %v", pid, err)
		}
		return set
	}

	// (1) A custom role granting exactly ONE support permission resolves to exactly
	// that permission — it grants its set and denies every other support action.
	t.Run("single permission grants exactly its set", func(t *testing.T) {
		for _, granted := range supportPerms {
			granted := granted
			t.Run(granted, func(t *testing.T) {
				perms := resolve(pt.single[granted])
				if !perms.Has(granted) {
					t.Errorf("role granting %q: Has(%q)=false, want true", granted, granted)
				}
				for _, other := range supportPerms {
					if other == granted {
						continue
					}
					if perms.Has(other) {
						t.Errorf("role granting only %q over-grants %q", granted, other)
					}
				}
				// The principal has no other membership, so the resolved set is
				// exactly {granted} — a tightening that catches leakage from
				// inheritance or a future implicit grant.
				if len(perms) != 1 {
					t.Errorf("role granting only %q resolved to %d perms %v, want exactly 1",
						granted, len(perms), sortedKeys(perms))
				}
			})
		}
	})

	// (2) Built-in presets resolve to the data-model.md matrix. owner is the locked
	// preset (resolver → whole catalog); admin is granted all six explicitly; member
	// gets day-to-day triage but not delete/inbox.manage; viewer is read-only. We
	// assert only the six support keys (presets also carry the 001 tenancy/iam keys).
	t.Run("preset matrix", func(t *testing.T) {
		cases := []struct {
			name string
			pid  uuid.UUID
			want map[string]bool
		}{
			{"owner", pt.owner, allSupport(true)},
			{"admin", pt.admin, allSupport(true)},
			{"member", pt.member, supportSet("tickets.read", "tickets.reply", "tickets.write", "tickets.assign")},
			{"viewer", pt.viewer, supportSet("tickets.read")},
		}
		for _, c := range cases {
			perms := resolve(c.pid)
			for _, key := range supportPerms {
				if got := perms.Has(key); got != c.want[key] {
					t.Errorf("%s: Has(%q)=%v, want %v", c.name, key, got, c.want[key])
				}
			}
		}
	})
}

// supportSet returns a {perm: true} map over the given keys (others implicitly false).
func supportSet(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// allSupport returns {every support perm: v}.
func allSupport(v bool) map[string]bool {
	m := make(map[string]bool, len(supportPerms))
	for _, k := range supportPerms {
		m[k] = v
	}
	return m
}

func sortedKeys(p authz.PermissionSet) []string {
	out := make([]string, 0, len(p))
	for k := range p {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
