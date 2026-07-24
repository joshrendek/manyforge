// Spec 006 Feedback / Feature-Request Boards — regression contracts (source-level pins).
//
// No build tag: these fast, DB-free pins run in both `make test` and `make sec-test`, so a
// refactor that silently drops a feedback protection fails CI loudly even when the behavioral
// integration matrix is skipped. They pin the five Spec-006 regression contracts:
//   1. tenant isolation      — business-scoped RLS (authorized_businesses) on every table +
//                              the tenant_root_id ownership predicate on every id-taking query;
//   2. voting integrity      — one vote per identity per post (unique index);
//   3. ticket-link integrity — the tenant-consistent composite FK to ticket;
//   4. public-portal oracle  — the publishable-key lookup filters enabled key AND public board,
//                              and the principal-less DEFINERs are search_path-pinned.
//   5. verified-identity tier (0104, saz.5) — constant-time signature compare + bounded replay
//      skew, the re-created DEFINERs stay search_path-pinned, the exactly-once idempotency table
//      is policy-less/grant-less (DEFINER-only), the FB409 conflict path, the backfill's
//      principal exclusion, resolveVerified's fail-closed matrix, and the plaintext secret never
//      reaching a logger.
package security_regression

import (
	"strings"
	"testing"
)

// TestPin_FeedbackRLSBusinessScoped asserts migration 0102 enables RLS and installs a policy on
// every feedback table, scoped by authorized_businesses(current_principal()) — NOT
// authorized_tenants. Feedback is business-scoped (like the support desk); an authorized_tenants
// predicate would make a board readable/writable across every business in the tenant tree — a
// cross-business hole. WITH CHECK is pinned too (2 occurrences per policy → >= 8).
func TestPin_FeedbackRLSBusinessScoped(t *testing.T) {
	mig := mustRead(t, "../../migrations/0102_feedback_boards.up.sql")
	for _, frag := range []string{
		"ALTER TABLE feedback_board ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE feedback_post ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE feedback_vote ENABLE ROW LEVEL SECURITY",
		"ALTER TABLE feedback_ingest_key ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY feedback_board_rls",
		"CREATE POLICY feedback_post_rls",
		"CREATE POLICY feedback_vote_rls",
		"CREATE POLICY feedback_ingest_key_rls",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0102_feedback_boards.up.sql: missing RLS fragment %q — was a feedback tenant-isolation policy dropped (contract #1)?", frag)
		}
	}
	// USING + WITH CHECK on all four policies → >= 8 occurrences of the business-scoped predicate.
	if n := strings.Count(mig, "authorized_businesses(current_principal())"); n < 8 {
		t.Errorf("0102: expected the business-scoped predicate on USING+WITH CHECK of all four policies (>=8), got %d — weakened, or a WITH CHECK dropped?", n)
	}
	// authorized_tenants would scope feedback per-tenant (cross-business hole). Check executable
	// SQL only (strip comments) so header prose can never trip a false positive.
	if strings.Contains(stripSQLComments(mig), "authorized_tenants") {
		t.Errorf("0102: feedback policies must scope by authorized_businesses, NOT authorized_tenants — a per-tenant predicate is a cross-business hole on business-scoped rows (contract #1)")
	}
}

// TestPin_FeedbackQueriesScopeByTenantRoot asserts every id-taking feedback query still carries
// the tenant_root_id ownership predicate in SQL (dual enforcement with RLS), so a foreign-tenant
// id matches zero rows ⇒ ErrNotFound (no existence oracle).
func TestPin_FeedbackQueriesScopeByTenantRoot(t *testing.T) {
	sql := mustRead(t, "../../db/query/feedback.sql")
	for _, q := range []string{
		"GetFeedbackBoard",
		"UpdateFeedbackBoard",
		"GetFeedbackPost",
		"SetFeedbackPostStatus",
		"SoftDeleteFeedbackPost",
		"IncrementFeedbackPostVoteCount",
		"RevokeFeedbackIngestKey",
	} {
		block := queryBlock(t, sql, q)
		if !strings.Contains(block, "tenant_root_id =") {
			t.Errorf("feedback.sql: %s no longer scopes by tenant_root_id (ownership predicate dropped — would rely on RLS alone, contract #1)", q)
		}
	}
}

// TestPin_FeedbackVotingIntegrity asserts the one-vote-per-identity unique index survives. Its
// loss would let a single identity inflate a post's vote_count without bound (contract #2). The
// public vote DEFINER relies on the ON CONFLICT against this exact constraint.
func TestPin_FeedbackVotingIntegrity(t *testing.T) {
	mig := mustRead(t, "../../migrations/0102_feedback_boards.up.sql")
	for _, frag := range []string{
		"UNIQUE (post_id, voter_identity)",
		"ON CONFLICT (post_id, voter_identity) DO NOTHING",
	} {
		if !strings.Contains(mig, frag) {
			t.Errorf("0102: missing voting-integrity fragment %q — one-vote-per-identity weakened (contract #2)?", frag)
		}
	}
}

// TestPin_FeedbackTicketLinkIntegrity asserts feedback_post links to ticket through the
// tenant-consistent composite FK, so a post can never be linked to a ticket in another tenant
// (contract #3).
func TestPin_FeedbackTicketLinkIntegrity(t *testing.T) {
	mig := mustRead(t, "../../migrations/0102_feedback_boards.up.sql")
	if !strings.Contains(mig, "FOREIGN KEY (ticket_id, tenant_root_id) REFERENCES ticket (id, tenant_root_id)") {
		t.Errorf("0102: feedback_post.ticket_id must be a composite FK to ticket (id, tenant_root_id) — a plain ticket_id FK would allow cross-tenant ticket links (contract #3)")
	}
}

// TestPin_FeedbackPublicOracleBoundary asserts the publishable-key lookup only returns a row for
// an ENABLED key on a PUBLIC board — the oracle boundary. If the enabled/is_public filters were
// dropped, a revoked key or a private board would resolve, leaking existence and re-enabling
// disabled ingest (contract #4).
func TestPin_FeedbackPublicOracleBoundary(t *testing.T) {
	mig := mustRead(t, "../../migrations/0102_feedback_boards.up.sql")
	block := funcBody(t, mig, "CREATE FUNCTION feedback_public_board(")
	for _, frag := range []string{
		"k.status = 'enabled'",
		"b.is_public",
	} {
		if !strings.Contains(block, frag) {
			t.Errorf("0102 feedback_public_board: missing oracle-boundary filter %q — a revoked key or private board would resolve (contract #4)", frag)
		}
	}
	// The public handler must answer a UNIFORM 401 (writeUnauthorized) for an unresolved key,
	// via the feedback_public_board lookup — never a distinct not-found vs unauthorized shape.
	pub := mustRead(t, "../../internal/feedback/public.go")
	for _, frag := range []string{
		"feedback_public_board(",
		"writeUnauthorized(",
	} {
		if !strings.Contains(pub, frag) {
			t.Errorf("internal/feedback/public.go: missing %q — the public ingress oracle boundary (uniform 401) may have regressed (contract #4)", frag)
		}
	}
}

// TestPin_FeedbackDefinersHardened asserts every SECURITY DEFINER function in migration 0102 is
// search_path-pinned. The DEFINERs run as the table-owning role to bypass ENABLE-not-FORCE RLS
// during principal-less ingest; an unpinned search_path lets a caller shadow referenced objects
// and execute as the owner (privilege escalation). Every DEFINER must have a matching SET.
func TestPin_FeedbackDefinersHardened(t *testing.T) {
	// Strip comments first: the header prose legitimately mentions "SECURITY DEFINER", which
	// would otherwise inflate the definer count over the (comment-free) SET clauses.
	mig := stripSQLComments(mustRead(t, "../../migrations/0102_feedback_boards.up.sql"))
	definers := strings.Count(mig, "SECURITY DEFINER")
	pinned := strings.Count(mig, "SET search_path = public")
	if definers == 0 {
		t.Fatalf("0102: expected SECURITY DEFINER functions for the public ingress path, found none")
	}
	if definers != pinned {
		t.Errorf("0102: %d SECURITY DEFINER functions but only %d have SET search_path = public — an unpinned DEFINER is a privesc vuln (contract #4)", definers, pinned)
	}
}

// --- 0104 verified-identity tier (saz.5) -----------------------------------------------------

// TestPin_FeedbackSignatureConstantTimeAndSkew asserts internal/feedback/signature.go compares
// the request MAC with hmac.Equal (constant-time) — a plain `==`/bytes.Equal comparison would
// reopen a byte-at-a-time timing side-channel on the shared secret — and that the replay window
// stays a bounded 300s (5 min). A widened/removed skew bound weakens replay protection for a
// captured signature.
func TestPin_FeedbackSignatureConstantTimeAndSkew(t *testing.T) {
	src := mustRead(t, "../../internal/feedback/signature.go")
	if !strings.Contains(src, "hmac.Equal(") {
		t.Errorf("signature.go: expected a constant-time hmac.Equal( compare — a non-constant-time compare reopens a timing side-channel on the signing secret")
	}
	if !strings.Contains(src, "sigMaxSkew = 300 * time.Second") {
		t.Errorf("signature.go: expected sigMaxSkew = 300 * time.Second (5-minute replay window) — a widened/removed skew bound weakens replay protection")
	}
}

// TestPin_FeedbackVerifiedIdentityDefinersHardened asserts migration 0104's four re-created
// DEFINER functions (feedback_public_board/_submit/_vote/_list_posts) each stay
// SECURITY DEFINER SET search_path = public. 0104 DROP+CREATEs every 0102 DEFINER to change its
// signature; an unpinned search_path on any of them lets a caller shadow a referenced object and
// execute as the table-owning role (privilege escalation).
func TestPin_FeedbackVerifiedIdentityDefinersHardened(t *testing.T) {
	mig := stripSQLComments(mustRead(t, "../../migrations/0104_feedback_verified_identity.up.sql"))
	definers := strings.Count(mig, "SECURITY DEFINER")
	pinned := strings.Count(mig, "SET search_path = public")
	if definers == 0 {
		t.Fatalf("0104: expected re-created SECURITY DEFINER functions, found none")
	}
	if definers != pinned {
		t.Errorf("0104: %d SECURITY DEFINER functions but only %d SET search_path = public — an unpinned DEFINER is a privesc vuln", definers, pinned)
	}
	if definers != 4 {
		t.Errorf("0104: expected exactly 4 re-created DEFINER functions (board/submit/vote/list_posts), got %d — was one dropped, or another added without this pin being updated?", definers)
	}
}

// TestPin_FeedbackIdempotencyTableLockedDown asserts feedback_ingest_idempotency has RLS enabled
// with NO policy and NO grant to manyforge_app — only the SECURITY DEFINER functions (which run
// as the table owner, bypassing RLS) may read/write it. Its idem_key values are attacker/customer
// supplied and often embed a device or order identifier; unlike the 0102 feedback tables (which
// DO carry per-tenant app policies + grants), a principal-scoped read path here would cross a
// boundary this table was deliberately designed not to have.
func TestPin_FeedbackIdempotencyTableLockedDown(t *testing.T) {
	mig := stripSQLComments(mustRead(t, "../../migrations/0104_feedback_verified_identity.up.sql"))
	if !strings.Contains(mig, "CREATE TABLE feedback_ingest_idempotency") {
		t.Fatalf("0104: feedback_ingest_idempotency table not found — was the exactly-once consumed-set dropped?")
	}
	if !strings.Contains(mig, "ALTER TABLE feedback_ingest_idempotency ENABLE ROW LEVEL SECURITY") {
		t.Errorf("0104: feedback_ingest_idempotency must ENABLE ROW LEVEL SECURITY")
	}
	// Collapse to whitespace-normalized, semicolon-delimited statements before scanning: this
	// codebase writes multi-line GRANTs (see migrations/0007_rls.up.sql: "GRANT ... ON\n
	// <tables>\n TO manyforge_app;"), so a same-line-only check would miss a multi-line
	// GRANT/POLICY naming this table — exactly the table this pin exists to protect.
	for _, stmt := range strings.Split(strings.Join(strings.Fields(mig), " "), ";") {
		if !strings.Contains(stmt, "feedback_ingest_idempotency") {
			continue
		}
		if strings.Contains(stmt, "CREATE POLICY") {
			t.Errorf("0104: found a CREATE POLICY naming feedback_ingest_idempotency (%q) — it must stay policy-less so only the DEFINERs (which bypass RLS) can touch it", stmt)
		}
		if strings.Contains(stmt, "GRANT") && strings.Contains(stmt, "manyforge_app") && !strings.Contains(stmt, "FUNCTION") {
			t.Errorf("0104: found a GRANT ... manyforge_app naming feedback_ingest_idempotency (%q) — the app role must have NO table privileges on it (function EXECUTE grants for the DEFINERs are fine)", stmt)
		}
	}
}

// TestPin_FeedbackSubmitConflictErrcode asserts feedback_public_submit still RAISEs the FB409
// errcode on a same-idempotency-key-different-body reuse. The handler's txErr branch in
// public.go matches on this exact code to answer 409 instead of a generic 500; losing the
// errcode silently downgrades a client-detectable conflict into an opaque internal error.
func TestPin_FeedbackSubmitConflictErrcode(t *testing.T) {
	mig := stripSQLComments(mustRead(t, "../../migrations/0104_feedback_verified_identity.up.sql"))
	// Pin the actual RAISE control, not a bare "FB409" token — a doc comment (e.g. "RAISEs
	// FB409 on same-key-different-body") would satisfy a bare-token check even if the real
	// USING ERRCODE = 'FB409' control were deleted.
	if !strings.Contains(mig, "ERRCODE = 'FB409'") {
		t.Error("0104 must RAISE ERRCODE 'FB409' on idempotency-key body mismatch (exactly-once contract)")
	}
}

// TestPin_FeedbackBackfillExcludesPrincipals asserts the 0104 vote backfill excludes rows whose
// voter_identity is a principal UUID (internal/authenticated votes) from the a: namespace
// rewrite. Losing this predicate would prefix an authenticated principal's vote row, breaking
// the identity match the internal Service.Vote path relies on (voter_identity = principalID
// verbatim).
func TestPin_FeedbackBackfillExcludesPrincipals(t *testing.T) {
	mig := stripSQLComments(mustRead(t, "../../migrations/0104_feedback_verified_identity.up.sql"))
	if !strings.Contains(mig, "NOT IN (SELECT id::text FROM principal)") {
		t.Errorf("0104: backfill no longer excludes principal UUIDs from the a: namespace rewrite — an internal vote row could be silently reprefixed")
	}
}

// TestPin_FeedbackResolveVerifiedFailClosed asserts public.go's resolveVerified answers the §3
// fail-closed matrix on BOTH paths where a signature is present but cannot be checked: the
// handler has no Sealer configured, and the Sealer fails to unseal the stored secret. Either
// collapsing to (verified=false, sigBad=false) — i.e. silently treating an unverifiable signed
// request as anonymous — would let a caller with a stale/guessed signature slip past the
// verified-identity gate instead of being rejected.
func TestPin_FeedbackResolveVerifiedFailClosed(t *testing.T) {
	src := mustRead(t, "../../internal/feedback/public.go")
	body := goFuncBody(t, src, "func (h *PublicHandler) resolveVerified(")
	// At least the two named branches, plus the present-but-invalid-signature branch, all
	// fail closed — a regression that collapses any of them to (false, false) (silently
	// anon) is the vulnerability this pin exists to catch.
	if n := strings.Count(body, "return false, true"); n < 2 {
		t.Errorf("public.go resolveVerified: expected >= 2 fail-closed `return false, true` branches, got %d — the fail-closed matrix may have regressed", n)
	}
	failClosedBranch(t, body, "h.Sealer == nil")
	failClosedBranch(t, body, "Sealer.Open(")
}

// failClosedBranch asserts the branch opened by marker returns false, true (fail-closed) within
// a short window after the marker — long enough to span the branch's log line + return, short
// enough that it can't accidentally match a sibling branch's return.
func failClosedBranch(t *testing.T, body, marker string) {
	t.Helper()
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("public.go resolveVerified: branch marker %q not found — was it renamed or removed?", marker)
	}
	window := body[idx:]
	if len(window) > 160 {
		window = window[:160]
	}
	if !strings.Contains(window, "return false, true") {
		t.Errorf("public.go resolveVerified: branch at %q does not fail closed (return false, true)", marker)
	}
}

// TestPin_FeedbackIngestSecretNeverLogged is a negative pin: no logger call in ingestkey.go or
// handler.go references the plaintext fbs_ secret (secretPlain / .Secret). The secret is
// write-once — surfaced only in CreateIngestKey's HTTP response — and must never additionally
// land in application logs.
func TestPin_FeedbackIngestSecretNeverLogged(t *testing.T) {
	for _, path := range []string{"../../internal/feedback/ingestkey.go", "../../internal/feedback/handler.go"} {
		src := mustRead(t, path)
		for _, ln := range strings.Split(src, "\n") {
			isLogCall := strings.Contains(ln, "Logger.") || strings.Contains(ln, "slog.") ||
				strings.Contains(ln, "Printf(") || strings.Contains(ln, "log.Print")
			if !isLogCall {
				continue
			}
			for _, bad := range []string{"secretPlain", "k.Secret", "out.Secret", ".Secret,", ".Secret)"} {
				if strings.Contains(ln, bad) {
					t.Errorf("%s: logger call appears to reference the plaintext secret (%q) on line %q — secrets must never be logged", path, bad, strings.TrimSpace(ln))
				}
			}
		}
	}
}

// goFuncBody returns the Go source of the named function/method: the text from its signature
// marker up to (but not including) the next top-level "\nfunc " (or EOF). Fails the test if the
// marker is absent.
func goFuncBody(t *testing.T, src, marker string) string {
	t.Helper()
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatalf("function marker %q not found — was it renamed or removed?", marker)
	}
	rest := src[start:]
	if idx := strings.Index(rest[len(marker):], "\nfunc "); idx >= 0 {
		return rest[:len(marker)+idx]
	}
	return rest
}

// funcBody returns the text of a CREATE FUNCTION block from its opening marker up to the closing
// `$$;` terminator (or EOF). Fails the test if the marker is absent.
func funcBody(t *testing.T, sql, marker string) string {
	t.Helper()
	start := strings.Index(sql, marker)
	if start < 0 {
		t.Fatalf("0102: function marker %q not found — was it renamed or removed?", marker)
	}
	rest := sql[start:]
	if end := strings.Index(rest, "$$;"); end >= 0 {
		return rest[:end]
	}
	return rest
}
