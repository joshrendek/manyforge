// manyforge-p20 — event ingest + time-series storage foundation regression contracts
// (source-level pins).
//
// No build tag: these fast, DB-free pins run in both `make test` and `make sec-test`, so a
// refactor that silently drops a p20 protection fails CI loudly even when the behavioral
// integration matrix is skipped. They pin eight regression contracts:
//
//  1. partition placement  — partitioning is keyed on ingested_at (a server clock), never on a
//     client-supplied column, so a hostile occurred_at cannot conjure
//     partitions or dodge retention;
//  2. child-grant boundary — no GRANT is ever issued on a partition child, which is what makes a
//     newly created partition unable to open an RLS hole;
//  3. business scoping     — telemetry is business-scoped (authorized_businesses), NOT
//     tenant-scoped, so a client cannot be read across a tenant tree;
//  4. rollup idempotency   — the rollup RECOMPUTES buckets rather than incrementing them, because
//     worker execution is at-least-once;
//  5. definer hygiene      — every principal-less SECURITY DEFINER function pins search_path;
//  6. revoked-key refusal  — the resolver excludes inactive/revoked clients, so a revoked key is
//     indistinguishable from one that never existed;
//  7. ingest oracle        — signature comparison is constant-time and every auth failure funnels
//     through one uniform 401 writer;
//  8. opt-in signing       — require_signature defaults FALSE and drives verification, so
//     configuring a master key cannot silently lock out embeddable SDK keys.
package security_regression

import (
	"regexp"
	"strings"
	"testing"
)

const p20Migration = "../../migrations/0105_timeseries_foundation.up.sql"

// TestPin_P20PartitionKeyIsIngestedAt asserts every partitioned table is ranged on ingested_at.
// Partitioning on a client-supplied timestamp would let a caller choose which partition its rows
// land in — and therefore escape the retention sweep entirely by claiming a far-future time.
func TestPin_P20PartitionKeyIsIngestedAt(t *testing.T) {
	mig := stripSQLComments(mustRead(t, p20Migration))

	partitionBy := regexp.MustCompile(`PARTITION BY RANGE \(([^)]*)\)`)
	matches := partitionBy.FindAllStringSubmatch(mig, -1)
	if len(matches) < 2 {
		t.Fatalf("expected at least 2 partitioned tables (analytics_event, crash_event), found %d",
			len(matches))
	}
	for _, m := range matches {
		if strings.TrimSpace(m[1]) != "ingested_at" {
			t.Errorf("PARTITION BY RANGE (%s): partition key MUST be ingested_at (server clock), "+
				"never a client-supplied column", m[1])
		}
	}

	// ingested_at must not be settable by the ingest functions — it has to fall to its DEFAULT.
	for _, fn := range []string{"telemetry_ingest_analytics", "telemetry_ingest_crash"} {
		body := functionBody(t, mig, fn)
		if strings.Contains(body, "ingested_at") {
			t.Errorf("%s references ingested_at; the column must fall to its now() default so a "+
				"caller cannot steer partition placement", fn)
		}
	}
}

// TestPin_P20NoGrantOnPartitionChild asserts grants are issued on partitioned PARENTS only.
// Postgres checks privileges on the parent for routed operations, so withholding child grants is
// what guarantees a freshly created partition has no reachable path that skips the parent's RLS
// policy. create_due_partitions must not grant on what it creates.
func TestPin_P20NoGrantOnPartitionChild(t *testing.T) {
	mig := stripSQLComments(mustRead(t, p20Migration))

	// A child partition is named <parent>_YYYYMMDD or <parent>_YYYYMM.
	childGrant := regexp.MustCompile(`(?i)GRANT[^;]*\bON\s+(analytics_event|crash_event)_\d{6,8}\b`)
	if loc := childGrant.FindString(mig); loc != "" {
		t.Errorf("GRANT on a partition child (%q): grants belong on the partitioned parent only", loc)
	}

	body := functionBody(t, mig, "create_due_partitions")
	if strings.Contains(strings.ToUpper(body), "GRANT") {
		t.Error("create_due_partitions issues a GRANT on the partition it creates; that would give " +
			"manyforge_app a path to a child table that bypasses the parent's RLS policy")
	}
}

// TestPin_P20TelemetryIsBusinessScoped asserts the RLS policies scope on business_id via
// authorized_businesses — NOT tenant_root_id via authorized_tenants. authorized_tenants returns
// every tenant root reachable from any membership, so a tenant-scoped predicate would make every
// client and every event in the tenant tree readable, not just the caller's own businesses.
func TestPin_P20TelemetryIsBusinessScoped(t *testing.T) {
	mig := stripSQLComments(mustRead(t, p20Migration))

	for _, table := range []string{
		"telemetry_client", "analytics_event", "crash_event", "analytics_event_daily",
	} {
		// Whitespace-insensitive: the migration column-aligns these statements.
		re := regexp.MustCompile(`ALTER TABLE\s+` + table + `\s+ENABLE ROW LEVEL SECURITY`)
		if !re.MatchString(mig) {
			t.Errorf("%s does not ENABLE ROW LEVEL SECURITY", table)
		}
	}

	if strings.Contains(mig, "authorized_tenants(") {
		t.Error("0105 uses authorized_tenants: telemetry is BUSINESS-scoped; a tenant predicate " +
			"exposes every business in the tenant tree")
	}
	if n := strings.Count(mig, "authorized_businesses(current_principal())"); n < 8 {
		t.Errorf("expected >= 8 authorized_businesses predicates (USING + WITH CHECK on 4 tables), got %d", n)
	}
}

// TestPin_P20RollupRecomputesNeverIncrements is the correctness pin that protects against silent
// double-counting. Worker execution is at-least-once, so a retried or replayed sweep MUST be a
// no-op; `count = count + excluded.count` would inflate totals invisibly under load.
func TestPin_P20RollupRecomputesNeverIncrements(t *testing.T) {
	mig := stripSQLComments(mustRead(t, p20Migration))
	body := functionBody(t, mig, "rollup_analytics_daily")

	if !strings.Contains(body, "DO UPDATE SET event_count = excluded.event_count") {
		t.Error("rollup_analytics_daily must upsert with `= excluded.event_count` (recompute), " +
			"so an at-least-once retry is idempotent")
	}
	incrementing := regexp.MustCompile(`event_count\s*=\s*(analytics_event_daily\.)?event_count\s*\+`)
	if incrementing.MatchString(body) {
		t.Error("rollup_analytics_daily INCREMENTS event_count; at-least-once execution makes this " +
			"double-count on every retry")
	}
	// The watermark must be advanced inside the same function (hence the same transaction) as the
	// upsert, or a crash between the two would skip a window permanently.
	if !strings.Contains(body, "UPDATE rollup_state SET watermark_ingested_at") {
		t.Error("rollup_analytics_daily must advance the watermark in the same transaction as the upsert")
	}
	// Sweeping must key off ingested_at; occurred_at is client-controlled and non-monotonic.
	if !strings.Contains(body, "ingested_at > lo") || !strings.Contains(body, "ingested_at <= hi") {
		t.Error("rollup_analytics_daily must sweep by ingested_at (monotonic, client-independent)")
	}
	// The read window must start BEFORE the stored watermark. A strictly forward-only window
	// permanently drops a write that committed after the previous sweep's snapshot but carries an
	// ingested_at below that sweep's cutoff.
	if !strings.Contains(body, "lo := wm - p_overlap") {
		t.Error("rollup_analytics_daily must re-scan a trailing overlap (lo := wm - p_overlap), or a " +
			"straggler commit is skipped silently and forever")
	}
}

// TestPin_P20DefinerFunctionsPinSearchPath asserts every SECURITY DEFINER function in 0105 sets
// search_path. A definer function without a pinned search_path can be hijacked by a caller-created
// schema shadowing a referenced object — and these functions run as the RLS-exempt owner.
func TestPin_P20DefinerFunctionsPinSearchPath(t *testing.T) {
	mig := stripSQLComments(mustRead(t, p20Migration))

	definers := strings.Count(mig, "SECURITY DEFINER")
	pinned := strings.Count(mig, "SECURITY DEFINER SET search_path = public")
	if definers == 0 {
		t.Fatal("expected SECURITY DEFINER functions in 0105")
	}
	if definers != pinned {
		t.Errorf("%d SECURITY DEFINER functions but only %d pin search_path", definers, pinned)
	}

	for _, fn := range []string{
		"create_due_partitions", "drop_expired_partitions", "telemetry_resolve_client",
		"telemetry_ingest_analytics", "telemetry_ingest_crash", "rollup_analytics_daily",
	} {
		if !strings.Contains(mig, "REVOKE ALL ON FUNCTION "+fn) {
			t.Errorf("%s is not REVOKEd from PUBLIC", fn)
		}
	}
}

// TestPin_P20ResolveExcludesRevokedClients asserts the key lookup filters on active status AND a
// null revoked_at. Returning a revoked client would let a revoked key keep ingesting; returning
// rows for it in a distinguishable way would be a client-existence oracle.
func TestPin_P20ResolveExcludesRevokedClients(t *testing.T) {
	mig := stripSQLComments(mustRead(t, p20Migration))
	body := functionBody(t, mig, "telemetry_resolve_client")

	for _, frag := range []string{
		"c.publishable_key = p_key",
		"c.status = 'active'",
		"c.revoked_at IS NULL",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("telemetry_resolve_client is missing %q; a revoked or inactive key must resolve "+
				"to zero rows, indistinguishable from an unknown key", frag)
		}
	}
}

// TestPin_P20IngestUsesConstantTimeCompareAndUniform401 pins the two properties that keep the
// public ingest endpoint from leaking: MAC comparison is constant-time (hmac.Equal wraps
// subtle.ConstantTimeCompare), and every auth failure funnels through a single 401 writer so no
// caller can distinguish unknown from revoked from bad-signature.
func TestPin_P20IngestUsesConstantTimeCompareAndUniform401(t *testing.T) {
	sig := mustRead(t, "../../internal/telemetry/signature.go")
	if !strings.Contains(sig, "hmac.Equal(") {
		t.Error("telemetry signature verification must compare with hmac.Equal (constant time); a " +
			"bytes.Equal / == compare leaks the MAC through timing")
	}

	pub := mustRead(t, "../../internal/telemetry/public.go")
	if !strings.Contains(pub, "func (h *PublicHandler) unauthorized(w http.ResponseWriter)") {
		t.Fatal("public.go must funnel every auth failure through a single unauthorized() writer")
	}
	// Every 401 in the handler must go through that writer, never a bespoke WriteJSON with a
	// reason-specific body.
	bespoke := regexp.MustCompile(`WriteJSON\(w,\s*http\.StatusUnauthorized`)
	if bespoke.MatchString(pub[strings.Index(pub, "func (h *PublicHandler) ingest"):]) {
		t.Error("ingest writes a bespoke 401 instead of using unauthorized(); a reason-specific " +
			"body is a client-existence oracle")
	}

	// The body cap must be set in the handler itself, not delegated to global middleware.
	if !strings.Contains(pub, "http.MaxBytesReader(w, r.Body, maxIngestBytes)") {
		t.Error("ingest must set its own MaxBytesReader cap (defense in depth beneath the global " +
			"request-size middleware)")
	}
}

// functionBody returns the text of a CREATE FUNCTION block, so a pin can assert on one function
// rather than the whole migration (which would let a fragment elsewhere satisfy the check).
func functionBody(t *testing.T, migration, name string) string {
	t.Helper()
	// Later migrations redefine functions with CREATE OR REPLACE, so both spellings must match or
	// a pin silently stops inspecting the function it is supposed to guard.
	start := strings.Index(migration, "CREATE FUNCTION "+name)
	if start < 0 {
		start = strings.Index(migration, "CREATE OR REPLACE FUNCTION "+name)
	}
	if start < 0 {
		t.Fatalf("migration does not define %s (checked CREATE and CREATE OR REPLACE)", name)
	}
	body, _, found := strings.Cut(migration[start:], "$$;")
	if !found {
		t.Fatalf("could not find the end of %s", name)
	}
	return body
}

// TestPin_P20SignatureIsOptInNotSecretDerived asserts that whether ingest demands an HMAC is driven
// by the client's explicit require_signature flag, defaulting to FALSE — never inferred from the
// presence of a sealed secret.
//
// This is not a style preference. mfk_ publishable keys exist to be embedded in app binaries and
// web pages; the mfs_ secret is server-to-server only and must never ship inside a client. If
// signature enforcement were derived from "this client has a secret", then configuring a master key
// for the deployment would mint a secret for every client and silently lock out every embeddable
// SDK — a total outage of the primary consumer, with no code change to blame it on.
func TestPin_P20SignatureIsOptInNotSecretDerived(t *testing.T) {
	mig := stripSQLComments(mustRead(t, p20Migration))

	if !regexp.MustCompile(`require_signature\s+boolean\s+NOT NULL\s+DEFAULT\s+false`).MatchString(mig) {
		t.Error("telemetry_client.require_signature must exist and default to FALSE; a true default " +
			"would demand a signature from embeddable SDK clients that cannot hold a secret")
	}
	// A client that demands a signature must have something to verify against.
	if !strings.Contains(mig, "CHECK (NOT require_signature OR sealed_secret IS NOT NULL)") {
		t.Error("missing the constraint that a signing client has a sealed_secret")
	}
	// The resolver must surface the flag, or the handler cannot honor it.
	body := functionBody(t, mig, "telemetry_resolve_client")
	if !strings.Contains(body, "c.require_signature") {
		t.Error("telemetry_resolve_client must return require_signature")
	}

	pub := mustRead(t, "../../internal/telemetry/public.go")
	if !strings.Contains(pub, "client.requireSignature ||") {
		t.Error("ingest must gate signature verification on client.requireSignature; gating on " +
			"sealedSecret != nil alone would force every embeddable key into signed mode")
	}

	// Minting must be opt-in too: a secret issued to every client is dead credential surface.
	cl := mustRead(t, "../../internal/telemetry/client.go")
	if !strings.Contains(cl, "if requireSignature {") {
		t.Error("CreateClient must mint an mfs_ secret only for a signing client")
	}
}
