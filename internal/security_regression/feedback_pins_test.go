// Spec 006 Feedback / Feature-Request Boards — regression contracts (source-level pins).
//
// No build tag: these fast, DB-free pins run in both `make test` and `make sec-test`, so a
// refactor that silently drops a feedback protection fails CI loudly even when the behavioral
// integration matrix is skipped. They pin the four Spec-006 regression contracts:
//   1. tenant isolation      — business-scoped RLS (authorized_businesses) on every table +
//                              the tenant_root_id ownership predicate on every id-taking query;
//   2. voting integrity      — one vote per identity per post (unique index);
//   3. ticket-link integrity — the tenant-consistent composite FK to ticket;
//   4. public-portal oracle  — the publishable-key lookup filters enabled key AND public board,
//                              and the principal-less DEFINERs are search_path-pinned.
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
