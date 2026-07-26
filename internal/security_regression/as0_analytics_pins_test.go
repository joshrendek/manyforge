// manyforge-as0 — web analytics privacy contracts (source-level pins).
//
// The privacy claims here are the product. "Cookieless, no cross-site tracking, no PII retention"
// is not a marketing line a tenant can verify by reading the dashboard — it is only true if these
// invariants hold, and a refactor can silently break every one of them without failing a
// behavioral test. These pins make that failure loud:
//
//  1. no raw PII columns   — analytics_event has no column for ip or user_agent, so the raw values
//     cannot be persisted even by accident;
//  2. salt is unreachable  — analytics_salt is granted to nobody, so a read-only SQL injection
//     anywhere in the app cannot start re-deriving visitor hashes;
//  3. salt is random       — generated with gen_random_bytes, never a constant or a timestamp;
//  4. salts expire         — purged past retention, which is what makes an aged-out hash
//     permanently un-derivable rather than merely awkward to reverse;
//  5. query strings dropped — paths are stripped before storage, since query strings routinely
//     carry tokens and email addresses;
//  6. snippet is cookieless — the shipped JS touches no cookie or storage API;
//  7. collect never leaks  — the endpoint answers 204 uniformly and logs no key, IP, or UA.
package security_regression

import (
	"regexp"
	"strings"
	"testing"
)

const (
	as0Migration = "../../migrations/0106_analytics_pageviews.up.sql"
	as0Collect   = "../../internal/analytics/collect.go"
	as0Snippet   = "../../internal/analytics/snippet.go"
)

// TestPin_AS0NoRawPIIColumns asserts the event table has nowhere to put a raw IP or User-Agent.
// The strongest form of "we don't store it" is having no column that could.
func TestPin_AS0NoRawPIIColumns(t *testing.T) {
	mig := stripSQLComments(mustRead(t, as0Migration))

	// The ALTER that adds pageview columns must not introduce a PII column.
	for _, forbidden := range []string{
		"ADD COLUMN ip", "ADD COLUMN client_ip", "ADD COLUMN remote_addr",
		"ADD COLUMN user_agent", "ADD COLUMN ua ", "ADD COLUMN fingerprint",
	} {
		if strings.Contains(strings.ToLower(mig), strings.ToLower(forbidden)) {
			t.Errorf("0106 adds %q to analytics_event; raw IP/UA must never be persisted", forbidden)
		}
	}

	// The collect function must take ip/ua as ARGUMENTS (transient) and use them only inside the
	// hash expression.
	body := functionBody(t, mig, "analytics_collect")
	if !strings.Contains(body, "p_ip") || !strings.Contains(body, "p_ua") {
		t.Fatal("analytics_collect should accept p_ip / p_ua as arguments")
	}
	if !strings.Contains(body, "sha256(") {
		t.Error("analytics_collect must hash its inputs rather than storing them")
	}
	// The INSERT column list must not name an ip/ua column.
	ins := body[strings.Index(body, "INSERT INTO analytics_event"):]
	if end := strings.Index(ins, ";"); end > 0 {
		ins = ins[:end]
	}
	for _, forbidden := range []string{"p_ip,", "p_ip)", "p_ua,", "p_ua)"} {
		// p_ip/p_ua may appear inside the sha256(...) expression, but never as a bare inserted
		// value. Detect a value-position occurrence by requiring it NOT be inside the hash call.
		idx := strings.Index(ins, forbidden)
		if idx < 0 {
			continue
		}
		hashStart := strings.Index(ins, "sha256(")
		if hashStart < 0 || idx < hashStart {
			t.Errorf("analytics_collect appears to insert %q directly into a column", forbidden)
		}
	}
}

// TestPin_AS0SaltIsUnreachableByAppRole asserts the daily salt is granted to no one and carries no
// RLS policy, so only the SECURITY DEFINER collect function can read it.
func TestPin_AS0SaltIsUnreachableByAppRole(t *testing.T) {
	mig := stripSQLComments(mustRead(t, as0Migration))

	if !strings.Contains(mig, "CREATE TABLE analytics_salt") {
		t.Fatal("analytics_salt is not defined")
	}
	if !regexp.MustCompile(`ALTER TABLE\s+analytics_salt\s+ENABLE ROW LEVEL SECURITY`).MatchString(mig) {
		t.Error("analytics_salt does not ENABLE ROW LEVEL SECURITY")
	}
	// No GRANT may name it. With RLS on and no policy and no grant, it is unreachable outside the
	// definer function — which is the entire point.
	if regexp.MustCompile(`(?i)GRANT[^;]*\bON\s+analytics_salt\b`).MatchString(mig) {
		t.Error("SECURITY REGRESSION: analytics_salt is GRANTed. A read-only SQL injection could " +
			"then re-derive visitor hashes from candidate IPs")
	}
	if regexp.MustCompile(`(?i)CREATE POLICY[^;]*\bON\s+analytics_salt\b`).MatchString(mig) {
		t.Error("analytics_salt has an RLS policy; it should be reachable only via SECURITY DEFINER")
	}
}

// TestPin_AS0SaltIsRandomAndExpires asserts the salt is cryptographically random and purged.
// A constant or predictable salt would make the visitor hash trivially reversible given a
// candidate IP list; a salt that is never deleted makes every historical hash reversible forever.
func TestPin_AS0SaltIsRandomAndExpires(t *testing.T) {
	mig := stripSQLComments(mustRead(t, as0Migration))

	collect := functionBody(t, mig, "analytics_collect")
	if !strings.Contains(collect, "gen_random_bytes(") {
		t.Error("the daily salt must come from gen_random_bytes; a constant or timestamp-derived " +
			"salt makes visitor hashes reversible")
	}

	purge := functionBody(t, mig, "purge_expired_analytics_salts")
	if !strings.Contains(purge, "DELETE FROM analytics_salt") {
		t.Error("purge_expired_analytics_salts must actually delete expired salts")
	}
	// And the maintenance worker has to call it, or the function is decoration.
	part := mustRead(t, "../../internal/platform/timeseries/partition.go")
	if !strings.Contains(part, "purge_expired_analytics_salts") {
		t.Error("the maintenance worker never calls purge_expired_analytics_salts; salts would " +
			"accumulate forever and every historical visitor hash would stay re-derivable")
	}
}

// TestPin_AS0PathsAreStripped asserts query strings and fragments never reach storage. A query
// string routinely carries session tokens, password-reset codes, and email addresses; persisting
// one would quietly turn a pageview table into a PII store.
func TestPin_AS0PathsAreStripped(t *testing.T) {
	src := mustRead(t, as0Collect)
	if !strings.Contains(src, `strings.IndexAny(p, "?#")`) {
		t.Error("normalizePath must strip the query string and fragment before storage")
	}
	if !strings.Contains(src, "normalizePath(") {
		t.Error("the collect handler must normalize the path before storing it")
	}
	if !strings.Contains(src, "referrerHost(") {
		t.Error("the collect handler must reduce the referrer to a host before storing it")
	}
}

// TestPin_AS0SnippetIsCookieless asserts the shipped JS touches no persistent identifier. This is
// the claim a tenant's users are actually relying on.
func TestPin_AS0SnippetIsCookieless(t *testing.T) {
	src := mustRead(t, as0Snippet)
	for _, forbidden := range []string{
		"document.cookie", "localStorage", "sessionStorage", "indexedDB", "navigator.credentials",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("SECURITY REGRESSION: the snippet references %s. The privacy model forbids "+
				"any persistent identifier", forbidden)
		}
	}
	if !strings.Contains(src, "doNotTrack") {
		t.Error("the snippet must honour doNotTrack")
	}
}

// TestPin_AS0CollectIsUniformAndQuiet asserts the public collect endpoint neither varies its
// response nor logs the values it is trusted not to keep.
func TestPin_AS0CollectIsUniformAndQuiet(t *testing.T) {
	src := mustRead(t, as0Collect)

	if !strings.Contains(src, "defer w.WriteHeader(http.StatusNoContent)") {
		t.Error("collect must answer 204 uniformly; a varying status confirms which keys exist")
	}
	// Any other explicit status write in this file would be a divergence.
	statuses := regexp.MustCompile(`WriteHeader\(http\.Status(\w+)\)`).FindAllStringSubmatch(src, -1)
	for _, m := range statuses {
		if m[1] != "NoContent" {
			t.Errorf("collect writes a %s status; every outcome must be indistinguishable", m[1])
		}
	}

	// The log line must not carry the key, IP, or UA.
	for _, forbidden := range []string{`"key", `, `"ip", `, `"ua", `, `"user_agent"`, `"path", `} {
		if strings.Contains(src, forbidden) {
			t.Errorf("collect logs %s; that re-introduces the data the hash exists to avoid storing",
				forbidden)
		}
	}
}

const as0Enrichment = "../../migrations/0107_analytics_enrichment.up.sql"

// TestPin_AS0EnrichmentStoresOnlyLowCardinalityDerivations asserts the enrichment columns hold
// BUCKETS, not the identifying strings they were derived from.
//
// This is the line the whole privacy model rests on. A full User-Agent is a fingerprint — enough
// entropy to re-identify a visitor across days with no cookie at all — while "Safari" is not. A
// raw IP identifies a household; "US" does not. Adding a column for the raw value, or for a
// finer-grained derivation like an exact OS build, silently converts a compliant analytics table
// into a tracking database.
func TestPin_AS0EnrichmentStoresOnlyLowCardinalityDerivations(t *testing.T) {
	mig := stripSQLComments(mustRead(t, as0Enrichment))

	for _, forbidden := range []string{
		"ADD COLUMN ip", "ADD COLUMN client_ip", "ADD COLUMN remote_addr",
		"ADD COLUMN user_agent", "ADD COLUMN ua ", "ADD COLUMN fingerprint",
		"ADD COLUMN query", "ADD COLUMN query_string", "ADD COLUMN referrer_url",
		"ADD COLUMN city", "ADD COLUMN postal", "ADD COLUMN latitude", "ADD COLUMN longitude",
	} {
		if strings.Contains(strings.ToLower(mig), strings.ToLower(forbidden)) {
			t.Errorf("0107 adds %q. Enrichment must store low-cardinality buckets only — that "+
				"column would make the table re-identifying", forbidden)
		}
	}

	// The collect function still takes ip/ua transiently and must still only hash them.
	body := functionBody(t, mig, "analytics_collect")
	if !strings.Contains(body, "sha256(") {
		t.Error("analytics_collect must still hash ip/ua rather than storing them")
	}
	ins := body[strings.Index(body, "INSERT INTO analytics_event"):]
	hashAt := strings.Index(ins, "sha256(")
	for _, arg := range []string{"p_ip", "p_ua"} {
		for i := 0; i < len(ins); {
			idx := strings.Index(ins[i:], arg)
			if idx < 0 {
				break
			}
			at := i + idx
			// Every occurrence must be inside the hash expression, never a bare inserted value.
			if hashAt < 0 || at < hashAt {
				t.Errorf("analytics_collect appears to insert %q outside the hash expression", arg)
			}
			i = at + len(arg)
		}
	}
}

// TestPin_AS0UTMIsAnAllowlist asserts campaign parsing names the three keys it wants rather than
// storing whatever the caller sent. A query string is where session tokens, password-reset codes,
// and email addresses live; a denylist there is a leak waiting for the next parameter name.
func TestPin_AS0UTMIsAnAllowlist(t *testing.T) {
	src := mustRead(t, "../../internal/analytics/enrich.go")
	for _, key := range []string{`v.Get("utm_source")`, `v.Get("utm_medium")`, `v.Get("utm_campaign")`} {
		if !strings.Contains(src, key) {
			t.Errorf("ParseUTM must read %s explicitly (allowlist), not iterate the query", key)
		}
	}
	// Iterating every parameter would be a denylist by construction.
	if regexp.MustCompile(`for\s+\w+,\s*\w+\s*:=\s*range\s+v\b`).MatchString(src) {
		t.Error("ParseUTM iterates the whole query string; it must name the keys it keeps")
	}

	snip := mustRead(t, "../../internal/analytics/snippet.go")
	if !strings.Contains(snip, "utm_source") || !strings.Contains(snip, "URLSearchParams") {
		t.Error("the snippet must extract utm keys by name")
	}
	if strings.Contains(snip, "q:location.search") {
		t.Error("PRIVACY: the snippet forwards the raw query string")
	}
}

// TestPin_AS0GeoIsOptionalAndTransient asserts country lookup never persists an IP and that the
// server still boots without a geo database — an optional feature must not become a hard
// dependency by accident.
func TestPin_AS0GeoIsOptionalAndTransient(t *testing.T) {
	src := mustRead(t, "../../internal/analytics/geo.go")
	if !strings.Contains(src, "if path == \"\" {") {
		t.Error("OpenMMDB must treat an empty path as 'no geo configured', not an error")
	}
	// A lookup miss must not log: this runs per pageview and would write client IPs into the logs.
	if strings.Contains(src, "logger.Warn") || strings.Contains(src, "logger.Error") {
		t.Error("geo lookup must not log per-request; that would put client IPs in the logs")
	}
	enrich := mustRead(t, "../../internal/analytics/enrich.go")
	for _, guard := range []string{"IsLoopback()", "IsPrivate()", "IsLinkLocalUnicast()"} {
		if !strings.Contains(enrich, guard) {
			t.Errorf("ResolveCountry must reject %s addresses rather than report a guess", guard)
		}
	}
}

const as0CustomEvents = "../../migrations/0109_analytics_custom_events.up.sql"

// TestPin_AS0CustomEventsCannotForgePageviews asserts the two properties that keep custom events
// from corrupting the headline numbers.
//
// A site controls its own event names. If 'pageview' were accepted through the event API, any
// site could inflate its own pageview count at will — and the number would disagree with the
// pageview rollup that every other panel is reconciled against, with no way to tell which is
// right. Equally, if a pageview rollup stopped filtering on name, every custom event would
// silently become a pageview.
func TestPin_AS0CustomEventsCannotForgePageviews(t *testing.T) {
	src := mustRead(t, "../../internal/analytics/enrich.go")
	if !strings.Contains(src, `reservedEventName = "pageview"`) {
		t.Error("'pageview' must be a reserved event name")
	}
	if !strings.Contains(src, "s == reservedEventName") {
		t.Error("eventNameOK must reject the reserved name")
	}

	mig := stripSQLComments(mustRead(t, as0CustomEvents))

	// Every pageview aggregate must still be scoped to name = 'pageview'.
	rollup := functionBody(t, mig, "rollup_analytics_dimensions")
	if !strings.Contains(rollup, "e.name = 'pageview'") {
		t.Error("the pageview-derived dimensions must stay filtered to name = 'pageview', or " +
			"custom events would be counted in the device/browser/country breakdowns and no " +
			"longer reconcile against the pageview total")
	}
	if !strings.Contains(rollup, "e.name <> 'pageview'") {
		t.Error("the event dimension must select only non-pageview rows")
	}

	// The pageview rollups in 0106 must be untouched by this migration.
	if strings.Contains(mig, "CREATE OR REPLACE FUNCTION rollup_analytics_pageviews") ||
		strings.Contains(mig, "CREATE FUNCTION rollup_analytics_pageviews") {
		t.Error("0109 redefines rollup_analytics_pageviews; custom events must not change how " +
			"pageviews are counted")
	}
}

// TestPin_AS0CustomEventPayloadIsBounded asserts event names and properties are bounded before
// they reach a GROUP BY key or a jsonb column. Both are attacker-supplied on a public endpoint.
func TestPin_AS0CustomEventPayloadIsBounded(t *testing.T) {
	src := mustRead(t, "../../internal/analytics/enrich.go")
	for _, need := range []string{
		"maxEventNameLen", "maxPropKeys", "maxPropKeyLen", "maxPropValueLen",
	} {
		if !strings.Contains(src, need) {
			t.Errorf("missing bound: %s", need)
		}
	}
	// Nested structures must be dropped rather than stringified into the column.
	if !strings.Contains(src, "// Nested objects and arrays are dropped") {
		t.Error("SanitizeProps must drop nested objects/arrays rather than serialising them")
	}
	// Key selection past the cap must be deterministic, or the same payload stores different
	// props on different requests.
	if !strings.Contains(src, "sort.Strings(keys)") {
		t.Error("prop key truncation must be deterministic (sorted), not map-order dependent")
	}
}

// TestPin_AS0SnippetNeverThrowsIntoHostPage asserts the custom-event API cannot raise into the
// embedding site's own code. This snippet runs on other people's websites; an exception escaping
// an analytics call is their bug report, not ours.
func TestPin_AS0SnippetNeverThrowsIntoHostPage(t *testing.T) {
	src := mustRead(t, as0Snippet)
	if !strings.Contains(src, "try{b=JSON.stringify(o);}catch(e){return;}") {
		t.Error("payload serialisation must be guarded — an unserialisable object must cost the " +
			"event, not throw into the host page")
	}
	i := strings.Index(src, "window.mf=function(n,d){")
	if i < 0 {
		t.Fatal("window.mf is not defined")
	}
	body := src[i : i+400]
	if !strings.Contains(body, "try{") || !strings.Contains(body, "catch(e){}") {
		t.Error("the window.mf body must be wrapped so it never throws into the host page")
	}
}
