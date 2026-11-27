//go:build integration

package portaldomain

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

type tenant struct {
	business  uuid.UUID
	principal uuid.UUID
}

type stubResolver struct {
	mu   sync.Mutex
	recs map[string][]string
	err  error
}

func (r *stubResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	return r.recs[name], nil
}

func (r *stubResolver) publish(name string, vals ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recs == nil {
		r.recs = map[string][]string{}
	}
	r.recs[name] = vals
}

func (r *stubResolver) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

type fixture struct {
	ctx  context.Context
	tdb  *testdb.TestDB
	svc  *Service
	dns  *stubResolver
	a, b tenant

	mu     sync.Mutex
	forgot []string
}

func (f *fixture) forgotten() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forgot...)
}

func seedTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB, name string) tenant {
	t.Helper()
	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("owner role: %v", err)
	}
	tn := tenant{business: uuid.New(), principal: uuid.New()}
	account := uuid.New()
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'O','active',now(),now(),now())`,
			[]any{account, "pd-" + account.String() + "@example.test"}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{tn.principal, account}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,$2,'active',now(),now())`, []any{tn.business, name}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{tn.business}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`, []any{tn.principal, tn.business, ownerRole}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return tn
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	f := &fixture{ctx: ctx, tdb: tdb, dns: &stubResolver{}}
	f.a = seedTenant(ctx, t, tdb, "AlphaCo")
	f.b = seedTenant(ctx, t, tdb, "BetaCo")
	f.svc = &Service{
		DB:           tdb.App,
		Resolver:     f.dns,
		InstanceHost: "app.example.test",
		Reserved:     []string{"mail.example.test", ""},
		Forget: func(h string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.forgot = append(f.forgot, h)
		},
	}
	return f
}

func (f *fixture) auditCount(t *testing.T, action string, business uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.tdb.Super.QueryRow(f.ctx,
		`SELECT count(*) FROM audit_entry WHERE action=$1 AND business_id=$2`, action, business).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

func (f *fixture) verifiedAt(t *testing.T, id uuid.UUID) *time.Time {
	t.Helper()
	var ts *time.Time
	if err := f.tdb.Super.QueryRow(f.ctx, `SELECT verified_at FROM portal_domain WHERE id=$1`, id).Scan(&ts); err != nil {
		t.Fatalf("verified_at: %v", err)
	}
	return ts
}

func TestCreateNormalisesAndReturnsChallenge(t *testing.T) {
	f := newFixture(t)
	d, err := f.svc.Create(f.ctx, f.a.principal, f.a.business, " Portal.Example.TEST. ")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if d.Hostname != "portal.example.test" {
		t.Errorf("hostname = %q", d.Hostname)
	}
	if d.Verification.Name != "_manyforge-portal.portal.example.test" {
		t.Errorf("challenge name = %q", d.Verification.Name)
	}
	if len(d.Verification.Value) <= len("mf-portal-verify=") || d.Verification.Value[:len("mf-portal-verify=")] != "mf-portal-verify=" {
		t.Errorf("challenge value = %q", d.Verification.Value)
	}
	if d.VerifiedAt != nil {
		t.Errorf("new domain must be unverified")
	}
	if n := f.auditCount(t, "portal_domain.created", f.a.business); n != 1 {
		t.Errorf("created audit rows = %d, want 1", n)
	}
}

func TestCreateRejects(t *testing.T) {
	f := newFixture(t)
	for _, h := range []string{
		"example", "*.example.com", "10.0.0.1", "::1", "-a.example.com", "a..example.com",
		"portal.example.c0m", "pörtal.example.com", "portal.example.com:443", "",
		"app.example.test", "mail.example.test", "support.mail.example.test",
		"localhost", "app.localhost", "api.ns.svc", "api.ns.svc.cluster.local",
	} {
		if _, err := f.svc.Create(f.ctx, f.a.principal, f.a.business, h); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("Create(%q) err = %v, want ErrValidation", h, err)
		}
	}
	if _, err := f.svc.Create(f.ctx, f.a.principal, f.a.business, "portal.example.com"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := f.svc.Create(f.ctx, f.a.principal, f.a.business, "Portal.Example.com"); !errors.Is(err, errs.ErrConflict) {
		t.Errorf("duplicate err = %v, want ErrConflict", err)
	}
	// A hostname merely ending in a reserved name without a label boundary is fine.
	if _, err := f.svc.Create(f.ctx, f.a.principal, f.a.business, "notmail.example.test"); err != nil {
		t.Errorf("non-boundary suffix rejected: %v", err)
	}
}

func TestVerify(t *testing.T) {
	f := newFixture(t)
	d, err := f.svc.Create(f.ctx, f.a.principal, f.a.business, "portal.example.test")
	if err != nil {
		t.Fatal(err)
	}
	verify := func() Domain {
		t.Helper()
		got, err := f.svc.Verify(f.ctx, f.a.principal, f.a.business, d.ID)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		return got
	}
	if got := verify(); got.VerifiedAt != nil {
		t.Error("no record must stay unverified")
	}
	f.dns.publish(d.Verification.Name, "mf-portal-verify=wrong")
	if got := verify(); got.VerifiedAt != nil {
		t.Error("wrong value must stay unverified")
	}
	f.dns.setErr(errors.New("servfail"))
	if got := verify(); got.VerifiedAt != nil {
		t.Error("resolver error must stay unverified")
	}
	f.dns.setErr(nil)
	if len(f.forgotten()) != 0 {
		t.Error("Forget called before verification")
	}

	f.dns.publish(d.Verification.Name, "other", d.Verification.Value)
	got := verify()
	if got.VerifiedAt == nil {
		t.Fatal("exact value must verify")
	}
	if n := f.auditCount(t, "portal_domain.verified", f.a.business); n != 1 {
		t.Errorf("verified audit rows = %d, want 1", n)
	}
	if fg := f.forgotten(); len(fg) != 1 || fg[0] != "portal.example.test" {
		t.Errorf("forgotten = %v", fg)
	}
	again := verify()
	if again.VerifiedAt == nil || !again.VerifiedAt.Equal(*got.VerifiedAt) {
		t.Error("re-verify must be a no-op success")
	}
	if n := f.auditCount(t, "portal_domain.verified", f.a.business); n != 1 {
		t.Errorf("re-verify wrote another audit row: %d", n)
	}
}

// twoPending creates the same hostname for both tenants and returns the domains.
func twoPending(t *testing.T, f *fixture) (Domain, Domain) {
	t.Helper()
	da, err := f.svc.Create(f.ctx, f.a.principal, f.a.business, "portal.example.test")
	if err != nil {
		t.Fatal(err)
	}
	db, err := f.svc.Create(f.ctx, f.b.principal, f.b.business, "portal.example.test")
	if err != nil {
		t.Fatal(err)
	}
	return da, db
}

func TestVerifiedHostnameIsExclusive(t *testing.T) {
	f := newFixture(t)
	da, db := twoPending(t, f)
	f.dns.publish(da.Verification.Name, da.Verification.Value, db.Verification.Value)
	if got, err := f.svc.Verify(f.ctx, f.a.principal, f.a.business, da.ID); err != nil || got.VerifiedAt == nil {
		t.Fatalf("first verify: %v %+v", err, got)
	}
	if _, err := f.svc.Verify(f.ctx, f.b.principal, f.b.business, db.ID); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("second verify err = %v, want ErrConflict", err)
	}
	if f.verifiedAt(t, db.ID) != nil {
		t.Error("second tenant must stay unverified")
	}
	if f.verifiedAt(t, da.ID) == nil {
		t.Error("incumbent must stay verified")
	}
}

func TestVerifyTakesOverStaleIncumbent(t *testing.T) {
	f := newFixture(t)
	da, db := twoPending(t, f)
	f.dns.publish(da.Verification.Name, da.Verification.Value)
	if _, err := f.svc.Verify(f.ctx, f.a.principal, f.a.business, da.ID); err != nil {
		t.Fatal(err)
	}
	before := len(f.forgotten())

	// The incumbent's record is gone, but the lookup fails: nothing may change.
	f.dns.publish(db.Verification.Name, db.Verification.Value)
	f.dns.setErr(errors.New("servfail"))
	got, err := f.svc.Verify(f.ctx, f.b.principal, f.b.business, db.ID)
	if err != nil || got.VerifiedAt != nil {
		t.Fatalf("resolver error: %v %+v", err, got)
	}
	if f.verifiedAt(t, da.ID) == nil || f.verifiedAt(t, db.ID) != nil {
		t.Fatal("resolver error must not displace anyone")
	}
	f.dns.setErr(nil)

	got, err = f.svc.Verify(f.ctx, f.b.principal, f.b.business, db.ID)
	if err != nil || got.VerifiedAt == nil {
		t.Fatalf("takeover verify: %v %+v", err, got)
	}
	if f.verifiedAt(t, da.ID) != nil {
		t.Error("incumbent must be un-verified")
	}
	var n int
	if err := f.tdb.Super.QueryRow(f.ctx, `SELECT count(*) FROM portal_domain WHERE id=$1`, da.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("incumbent row must not be deleted: n=%d err=%v", n, err)
	}
	if c := f.auditCount(t, "portal_domain.verified", f.b.business); c != 1 {
		t.Errorf("claimant verified audit = %d", c)
	}
	if c := f.auditCount(t, "portal_domain.verification_revoked", f.a.business); c != 1 {
		t.Errorf("incumbent revoked audit = %d", c)
	}
	if len(f.forgotten()) <= before {
		t.Error("Forget must be called on takeover")
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	da, db := twoPending(t, f)
	f.dns.publish(da.Verification.Name, da.Verification.Value, db.Verification.Value)
	if _, err := f.svc.Verify(f.ctx, f.a.principal, f.a.business, da.ID); err != nil {
		t.Fatal(err)
	}
	before := len(f.forgotten())
	if err := f.svc.Delete(f.ctx, f.a.principal, f.a.business, da.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	if err := f.tdb.Super.QueryRow(f.ctx, `SELECT count(*) FROM portal_domain WHERE id=$1`, da.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("row must be gone: n=%d err=%v", n, err)
	}
	if c := f.auditCount(t, "portal_domain.deleted", f.a.business); c != 1 {
		t.Errorf("deleted audit = %d", c)
	}
	if len(f.forgotten()) <= before {
		t.Error("Forget must be called on delete")
	}
	if got, err := f.svc.Verify(f.ctx, f.b.principal, f.b.business, db.ID); err != nil || got.VerifiedAt == nil {
		t.Errorf("other tenant must now verify: %v %+v", err, got)
	}
}

func TestTenantIsolation(t *testing.T) {
	f := newFixture(t)
	da, err := f.svc.Create(f.ctx, f.a.principal, f.a.business, "portal.example.test")
	if err != nil {
		t.Fatal(err)
	}
	// Tenant B's principal asks about tenant A's business with a real id.
	_, errReal := f.svc.Verify(f.ctx, f.b.principal, f.a.business, da.ID)
	_, errUnknown := f.svc.Verify(f.ctx, f.b.principal, f.a.business, uuid.New())
	if !errors.Is(errReal, errs.ErrNotFound) || !errors.Is(errUnknown, errs.ErrNotFound) {
		t.Errorf("verify: real=%v unknown=%v, want ErrNotFound", errReal, errUnknown)
	}
	delReal := f.svc.Delete(f.ctx, f.b.principal, f.a.business, da.ID)
	delUnknown := f.svc.Delete(f.ctx, f.b.principal, f.a.business, uuid.New())
	if !errors.Is(delReal, errs.ErrNotFound) || !errors.Is(delUnknown, errs.ErrNotFound) {
		t.Errorf("delete: real=%v unknown=%v, want ErrNotFound", delReal, delUnknown)
	}
	// Own business id with a foreign domain id is the same shape.
	if _, err := f.svc.Verify(f.ctx, f.b.principal, f.b.business, da.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("foreign id under own business: %v", err)
	}
	list, next, err := f.svc.List(f.ctx, f.b.principal, f.a.business, "", 10)
	if err != nil || len(list) != 0 || next != "" {
		t.Errorf("list = %v %q %v, want empty", list, next, err)
	}
	if f.verifiedAt(t, da.ID) != nil {
		t.Error("unexpected verification")
	}
}

func TestListPaginates(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		if _, err := f.svc.Create(f.ctx, f.a.principal, f.a.business, fmt.Sprintf("p%d.example.test", i)); err != nil {
			t.Fatal(err)
		}
	}
	page1, next, err := f.svc.List(f.ctx, f.a.principal, f.a.business, "", 2)
	if err != nil || len(page1) != 2 || next == "" {
		t.Fatalf("page1 = %d next=%q err=%v", len(page1), next, err)
	}
	page2, next2, err := f.svc.List(f.ctx, f.a.principal, f.a.business, next, 2)
	if err != nil || len(page2) != 1 || next2 != "" {
		t.Fatalf("page2 = %d next=%q err=%v", len(page2), next2, err)
	}
	if page2[0].Hostname != "p2.example.test" || page2[0].Verification.Name == "" {
		t.Errorf("page2[0] = %+v", page2[0])
	}
	if _, _, err := f.svc.List(f.ctx, f.a.principal, f.a.business, "garbage", 2); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("bad cursor err = %v", err)
	}
}

func TestBaseURLFor(t *testing.T) {
	f := newFixture(t)
	if u, ok := f.svc.BaseURLFor(f.ctx, f.a.business); ok || u != "" {
		t.Errorf("none verified: %q %v", u, ok)
	}
	d1, _ := f.svc.Create(f.ctx, f.a.principal, f.a.business, "one.example.test")
	d2, _ := f.svc.Create(f.ctx, f.a.principal, f.a.business, "two.example.test")
	f.dns.publish(d2.Verification.Name, d2.Verification.Value)
	if _, err := f.svc.Verify(f.ctx, f.a.principal, f.a.business, d2.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	f.dns.publish(d1.Verification.Name, d1.Verification.Value)
	if _, err := f.svc.Verify(f.ctx, f.a.principal, f.a.business, d1.ID); err != nil {
		t.Fatal(err)
	}
	if u, ok := f.svc.BaseURLFor(f.ctx, f.a.business); !ok || u != "https://two.example.test" {
		t.Errorf("BaseURLFor = %q %v, want the earlier verified (two)", u, ok)
	}
}
