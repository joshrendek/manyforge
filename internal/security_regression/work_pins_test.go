// Spec 018 Work Board — regression contracts (source-level pins).
//
// No build tag: these DB-free pins run in both `make test` and `make sec-test`, so a refactor
// that silently drops a Work Board protection fails CI even when the Docker-gated behavioral
// tests (internal/work/work_integration_test.go) are skipped. Contracts:
//  1. tenant isolation — business-scoped RLS (authorized_businesses) on every work table, and
//     the tenant_root_id ownership predicate on every non-INSERT query in db/query/work.sql;
//  2. work tokens are stored and looked up only as SHA-256 hashes — the plaintext never
//     reaches SQL, no query returns token_hash, and resolution goes through a
//     search_path-pinned SECURITY DEFINER that filters revoked tokens;
//  3. `mf sync` re-authorizes on every call — it runs WithPrincipal(issuer) and re-checks an
//     active account holding work.write; every token failure is the same 401;
//  4. rev ordering — every task write holds the project row lock (FOR UPDATE).
package security_regression

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

const workMigration = "../../migrations/0145_work_board.up.sql"

func TestPin_WorkRLSBusinessScoped(t *testing.T) {
	mig := mustRead(t, workMigration)
	for _, table := range []string{"work_project", "work_task", "work_token"} {
		for _, frag := range []string{
			"ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY",
			"CREATE POLICY " + table + "_rls ON " + table + " FOR ALL",
			"CREATE TRIGGER " + table + "_troot_immutable BEFORE UPDATE ON " + table,
			"('" + table + "', 'work', 'drain_fence_then_rewrite', 1)",
		} {
			if !strings.Contains(mig, frag) {
				t.Errorf("0145: missing %q — was a work-table isolation guard dropped (contract #1)?", frag)
			}
		}
	}
	if n := strings.Count(mig, "business_id IN (SELECT business_id FROM authorized_businesses(current_principal()))"); n < 6 {
		t.Errorf("0145: business-scoped predicate on USING+WITH CHECK of all three policies expected (>=6), got %d", n)
	}
	if strings.Contains(stripSQLComments(mig), "authorized_tenants") {
		t.Errorf("0145: work policies must scope by authorized_businesses, NOT authorized_tenants (cross-business hole)")
	}
}

func TestPin_WorkQueriesScopeByTenantRoot(t *testing.T) {
	sql := mustRead(t, "../../db/query/work.sql")
	names := regexp.MustCompile(`-- name: (\w+) `).FindAllStringSubmatch(sql, -1)
	if len(names) < 15 {
		t.Fatalf("work.sql: expected the full query set, found %d queries", len(names))
	}
	for _, m := range names {
		// queryBlock starts at the ":one"/":many" annotation; the statement follows it.
		_, stmt, _ := strings.Cut(queryBlock(t, sql, m[1]), "\n")
		block := stripSQLComments(stmt)
		if strings.HasPrefix(strings.TrimSpace(block), "INSERT") {
			continue // inserts carry tenant_root_id as a column value, checked by RLS WITH CHECK + composite FKs
		}
		if !strings.Contains(block, "tenant_root_id =") {
			t.Errorf("work.sql: %s does not scope by tenant_root_id (ownership predicate dropped, contract #1)", m[1])
		}
	}
	if !strings.Contains(queryBlock(t, sql, "LockWorkProject"), "FOR UPDATE") {
		t.Errorf("work.sql: LockWorkProject lost FOR UPDATE — rev order would no longer equal commit order (contract #4)")
	}
}

func TestPin_WorkTokenHashedNeverPlaintext(t *testing.T) {
	mig := mustRead(t, workMigration)
	for _, frag := range []string{
		"CONSTRAINT work_token_hash_uniq UNIQUE (token_hash)",
		"CONSTRAINT work_token_hash_chk CHECK (token_hash ~ '^[0-9a-f]{64}$')",
		"LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$",
		"WHERE t.token_hash = p_hash AND t.revoked_at IS NULL;",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0145: missing %q (contract #2)", frag)
		}
	}
	if !regexp.MustCompile(`(?m)^REVOKE ALL ON FUNCTION work_resolve_token\(text\) FROM PUBLIC;$`).MatchString(mig) ||
		!regexp.MustCompile(`(?m)^GRANT EXECUTE ON FUNCTION work_resolve_token\(text\) TO manyforge_app;$`).MatchString(mig) {
		t.Errorf("0145: work_resolve_token must be REVOKEd from PUBLIC and granted only to manyforge_app")
	}

	// The plaintext is hashed before it reaches SQL, on both the mint and the resolve path.
	svc := mustRead(t, "../work/service.go")
	if !strings.Contains(svc, "TokenHash: auth.HashToken(plaintext)") {
		t.Errorf("work/service.go: CreateToken must store auth.HashToken(plaintext), never the plaintext")
	}
	sync := mustRead(t, "../work/sync.go")
	for _, frag := range []string{
		"hash := auth.HashToken(plaintext)",
		"FROM work_resolve_token($1)`, hash,",
	} {
		if !strings.Contains(sync, frag) {
			t.Errorf("work/sync.go: token resolution lost %q — the plaintext must never be sent to SQL", frag)
		}
	}

	// No read path returns the stored hash: the only token_hash mention in executable SQL is
	// InsertWorkToken's column list, and the generated row types carry no TokenHash field.
	if n := strings.Count(stripSQLComments(mustRead(t, "../../db/query/work.sql")), "token_hash"); n != 1 {
		t.Errorf("work.sql mentions token_hash %d times, want 1 (the INSERT column list) — a query now returns or filters on it", n)
	}
	for _, row := range []any{dbgen.InsertWorkTokenRow{}, dbgen.ListWorkTokensRow{}, dbgen.RevokeWorkTokenRow{}} {
		if _, ok := reflect.TypeOf(row).FieldByName("TokenHash"); ok {
			t.Errorf("%T exposes TokenHash — token hashes must never leave the database", row)
		}
	}
	// The plaintext is never logged by the sync path.
	for _, f := range []string{"../work/sync.go", "../work/sync_handler.go"} {
		if strings.Contains(mustRead(t, f), "slog") {
			t.Errorf("%s logs — keep the token-bearing sync path log-free so a bearer token cannot leak", f)
		}
	}
}

func TestPin_WorkSyncReauthorizesIssuer(t *testing.T) {
	sync := mustRead(t, "../work/sync.go")
	for _, frag := range []string{
		"s.DB.WithPrincipal(ctx, a.PrincipalID,",
		"authz.Resolve(ctx, tx, a.PrincipalID, a.BusinessID)",
		"!perms.Has(authz.PermWorkWrite)",
		`acc.Status != "active"`,
		"q.LockWorkProject(ctx,",
	} {
		if !strings.Contains(sync, frag) {
			t.Errorf("work/sync.go: missing %q — a token must die with its issuer's access (contract #3)", frag)
		}
	}
	h := mustRead(t, "../work/sync_handler.go")
	for _, frag := range []string{
		`httpx.ErrorBody{Code: "UNAUTHORIZED", Message: "unauthorized"}`,
		"if errors.Is(err, errTokenUnauthorized) {\n\t\twriteUnauthorized(w)",
	} {
		if !strings.Contains(h, frag) {
			t.Errorf("work/sync_handler.go: missing %q — token failures must share one 401 (contract #3)", frag)
		}
	}
	// Web task writes take the same project lock as sync (contract #4).
	svc := mustRead(t, "../work/service.go")
	if n := strings.Count(svc, "loadProject(ctx, dbgen.New(tx), businessID, ref, true)") +
		strings.Count(svc, "loadProject(ctx, q, businessID, ref, true)"); n < 2 {
		t.Errorf("work/service.go: CreateTask and UpdateTask must load the project with lock=true, found %d", n)
	}
}
