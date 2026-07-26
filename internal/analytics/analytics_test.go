package analytics

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
)

func TestNormalizePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/", "/"},
		{"", "/"},
		{"docs", "/docs"},
		{"/docs/", "/docs"},
		{"/docs///", "/docs"},
		// The query string is the important one: it routinely carries session tokens, email
		// addresses, and password-reset codes. Storing it would quietly turn a pageview table into
		// a PII store.
		{"/search?q=secret&token=abc123", "/search"},
		{"/page#section", "/page"},
		{"/page?a=1#b", "/page"},
		{"/a/b/c", "/a/b/c"},
	}
	for _, c := range cases {
		if got := normalizePath(c.in); got != c.want {
			t.Errorf("normalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizePath_BoundsLength(t *testing.T) {
	long := "/" + strings.Repeat("x", maxPathLen*3)
	got := normalizePath(long)
	if len(got) > maxPathLen {
		t.Fatalf("path not bounded: got %d chars, cap is %d", len(got), maxPathLen)
	}
}

func TestReferrerHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"example.com", "example.com"},
		{"www.example.com", "example.com"},
		{"WWW.Example.COM", "example.com"},
		// A full URL must be reduced to its host: the endpoint is public, so a caller can send
		// anything, and a referring URL can carry paths and tokens.
		{"https://news.example.com/story?utm=x", "news.example.com"},
		{"http://example.com/a/b", "example.com"},
		// Junk and injection attempts collapse to empty rather than being stored.
		{"not a host", ""},
		{"exa mple.com", ""},
		{"'; DROP TABLE analytics_event; --", ""},
		{strings.Repeat("a", 300), ""},
	}
	for _, c := range cases {
		if got := referrerHost(c.in); got != c.want {
			t.Errorf("referrerHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsBot(t *testing.T) {
	bots := []string{
		"",
		"Googlebot/2.1 (+http://www.google.com/bot.html)",
		"Mozilla/5.0 (compatible; bingbot/2.0)",
		"curl/8.4.0",
		"python-requests/2.31.0",
		"Go-http-client/1.1",
		"Mozilla/5.0 HeadlessChrome/120",
		"Mozilla/5.0 (compatible; AhrefsBot/7.0)",
	}
	for _, ua := range bots {
		if !IsBot(ua) {
			t.Errorf("IsBot(%q) = false, want true", ua)
		}
	}

	humans := []string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Version/17.0 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:121.0) Gecko/20100101 Firefox/121.0",
	}
	for _, ua := range humans {
		if IsBot(ua) {
			t.Errorf("IsBot(%q) = true, want false — a real browser was filtered out", ua)
		}
	}
}

// An empty User-Agent is treated as a bot. A real browser always sends one, so an empty UA is a
// script or a stripped proxy — and counting those inflates a small site's numbers most.
func TestIsBot_EmptyUAIsBot(t *testing.T) {
	if !IsBot("") {
		t.Fatal("empty User-Agent should be treated as a bot")
	}
}

// The snippet is shipped to third-party sites, so a syntax error is an outage on someone else's
// page. These pin the properties that matter rather than the exact text.
func TestSnippetJS_Invariants(t *testing.T) {
	if strings.Count(snippetJS, "(") != strings.Count(snippetJS, ")") {
		t.Error("unbalanced parentheses in snippet")
	}
	if strings.Count(snippetJS, "{") != strings.Count(snippetJS, "}") {
		t.Error("unbalanced braces in snippet")
	}
	for _, must := range []string{
		"data-key",   // reads its own key
		"doNotTrack", // honours DNT
		"sendBeacon", // survives tab close
		"popstate",   // SPA navigation
		"pushState",  //
		"localhost",  // skips local dev
		"/a/e",       // posts to the collect endpoint
	} {
		if !strings.Contains(snippetJS, must) {
			t.Errorf("snippet is missing %q", must)
		}
	}
	// replaceState must NOT be tracked: SPAs use it for query-string rewrites, which are not
	// pageviews, and wrapping it silently inflates every SPA's numbers.
	if strings.Contains(snippetJS, "history.replaceState=") {
		t.Error("snippet wraps replaceState; that counts query-string rewrites as pageviews")
	}
	// The snippet must never read cookies or storage — that is the entire privacy claim.
	for _, forbidden := range []string{"document.cookie", "localStorage", "sessionStorage", "indexedDB"} {
		if strings.Contains(snippetJS, forbidden) {
			t.Errorf("snippet touches %s; the privacy model forbids any persistent identifier", forbidden)
		}
	}
}

// ---------------------------------------------------------------------------
// Enrichment
// ---------------------------------------------------------------------------

func TestParseUTM_AllowlistsOnlyCampaignKeys(t *testing.T) {
	// The critical property: everything that is not a utm_* key is discarded. A query string is
	// where session tokens and email addresses live, so anything leaking through here would turn
	// the pageview table into a PII store.
	u := ParseUTM("utm_source=hn&utm_medium=social&utm_campaign=launch" +
		"&token=SUPERSECRET&email=alice@example.com&session=abc123")
	if u.Source != "hn" || u.Medium != "social" || u.Campaign != "launch" {
		t.Fatalf("utm not parsed: %+v", u)
	}
	joined := u.Source + u.Medium + u.Campaign
	for _, leaked := range []string{"SUPERSECRET", "alice@example.com", "abc123"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("PRIVACY: %q leaked through UTM parsing", leaked)
		}
	}
}

func TestParseUTM_EdgeCases(t *testing.T) {
	if got := ParseUTM(""); got != (UTM{}) {
		t.Errorf("empty query: %+v", got)
	}
	if got := ParseUTM("?utm_source=hn"); got.Source != "hn" {
		t.Errorf("leading ? not handled: %+v", got)
	}
	if got := ParseUTM("utm_source=%zz"); got != (UTM{}) {
		t.Errorf("malformed query should yield nothing: %+v", got)
	}
	// Values are bounded: they become GROUP BY keys in the rollup.
	long := ParseUTM("utm_campaign=" + strings.Repeat("x", maxDimensionLen*3))
	if len(long.Campaign) > maxDimensionLen {
		t.Errorf("campaign not bounded: %d chars", len(long.Campaign))
	}
	// Control characters are stripped rather than stored.
	if got := ParseUTM("utm_source=a%00b%09c").Source; strings.ContainsAny(got, "\x00\t") {
		t.Errorf("control characters survived: %q", got)
	}
}

func TestDeviceType(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148": "mobile",
		"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15":                        "tablet",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Mobile Safari/537.36":          "mobile",
		// Android without "Mobile" is the conventional tablet signal.
		"Mozilla/5.0 (Linux; Android 14; SM-X200) AppleWebKit/537.36 Safari/537.36":                "tablet",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/120 Safari/537": "desktop",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Gecko/20100101 Firefox/121.0":                   "desktop",
		"": "",
	}
	for ua, want := range cases {
		if got := DeviceType(ua); got != want {
			t.Errorf("DeviceType(%.50q) = %q, want %q", ua, got, want)
		}
	}
}

// User-Agent strings lie: Edge claims Chrome AND Safari, Chrome claims Safari. Order matters, and
// getting it wrong silently reports every Edge user as Chrome.
func TestBrowser_HandlesImpersonation(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/120 Safari/537.36 Edg/120.0":     "Edge",
		"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/120 Safari/537.36 OPR/106.0":     "Opera",
		"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 SamsungBrowser/23 Chrome/115 Safari":  "Samsung Internet",
		"Mozilla/5.0 (Macintosh) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36": "Chrome",
		"Mozilla/5.0 (Macintosh) AppleWebKit/605.1.15 Version/17.0 Safari/605.1.15":               "Safari",
		"Mozilla/5.0 (Windows NT 10.0; rv:121.0) Gecko/20100101 Firefox/121.0":                    "Firefox",
		"SomeUnknownAgent/1.0": "Other",
		"":                     "",
	}
	for ua, want := range cases {
		if got := Browser(ua); got != want {
			t.Errorf("Browser(%.60q) = %q, want %q", ua, got, want)
		}
	}
}

func TestCloudflareCountry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted bool
		values  []string
		want    string
	}{
		{name: "trusted uppercase", trusted: true, values: []string{"US"}, want: "US"},
		{name: "trusted lowercase", trusted: true, values: []string{"ca"}, want: "CA"},
		{name: "surrounding whitespace", trusted: true, values: []string{" gb "}, want: "GB"},
		{name: "disabled ignores plausible value", trusted: false, values: []string{"US"}},
		{name: "missing", trusted: true},
		{name: "unknown", trusted: true, values: []string{"XX"}},
		{name: "tor", trusted: true, values: []string{"T1"}},
		{name: "comma separated", trusted: true, values: []string{"US, CA"}},
		{name: "duplicate headers", trusted: true, values: []string{"US", "CA"}},
		{name: "too long", trusted: true, values: []string{"USA"}},
		{name: "non ascii", trusted: true, values: []string{"ÉU"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cloudflareCountry(tc.trusted, tc.values); got != tc.want {
				t.Errorf("cloudflareCountry(%t, %q) = %q, want %q",
					tc.trusted, tc.values, got, tc.want)
			}
		})
	}
}

// The snippet must send only campaign keys — never the page's whole query string.
func TestSnippetJS_SendsOnlyUTMKeys(t *testing.T) {
	for _, must := range []string{"utm_source", "utm_medium", "utm_campaign", "URLSearchParams"} {
		if !strings.Contains(snippetJS, must) {
			t.Errorf("snippet missing %q", must)
		}
	}
	// location.search must only ever be READ through the allowlist, never forwarded wholesale.
	if strings.Contains(snippetJS, "q:location.search") || strings.Contains(snippetJS, "q=location.search") {
		t.Error("PRIVACY: the snippet forwards the raw query string, which carries session tokens")
	}
}

// Public input may not be valid UTF-8, and byte-slicing a multibyte value can create invalid UTF-8
// even from valid input. Postgres rejects invalid UTF-8 outright — and because collect answers 204
// unconditionally, that rejection would silently DISCARD the pageview with no visible symptom.
func TestClampDimension_AlwaysProducesValidUTF8(t *testing.T) {
	cases := []string{
		strings.Repeat("é", maxDimensionLen),   // multibyte, cut lands mid-rune
		strings.Repeat("日本語", maxDimensionLen), // 3-byte runes
		strings.Repeat("🎉", maxDimensionLen),   // 4-byte runes
		"campaign-" + strings.Repeat("ü", maxDimensionLen),
		"\xff\xfe invalid bytes",                   // invalid to begin with
		strings.Repeat("a", maxDimensionLen) + "é", // boundary lands exactly on the rune
	}
	for _, in := range cases {
		got := clampDimension(in)
		if !utf8.ValidString(got) {
			t.Errorf("clampDimension produced invalid UTF-8 for %.20q: %q", in, got)
		}
		if len(got) > maxDimensionLen {
			t.Errorf("clampDimension exceeded the cap for %.20q: %d bytes", in, len(got))
		}
	}
}

func TestParseUTM_InvalidUTF8IsSanitizedNotDropped(t *testing.T) {
	// A campaign tag with one bad byte should still yield a usable value rather than losing the
	// pageview.
	u := ParseUTM("utm_source=" + url.QueryEscape("hn\xffnews"))
	if !utf8.ValidString(u.Source) {
		t.Fatalf("invalid UTF-8 survived: %q", u.Source)
	}
	if u.Source == "" {
		t.Fatal("a single bad byte should be stripped, not discard the whole value")
	}
}

// ---------------------------------------------------------------------------
// Custom events
// ---------------------------------------------------------------------------

func TestNormalizeEventName(t *testing.T) {
	ok := []string{"grow_start", "grow-exit", "checkout.step", "ns:event", "A1", "a"}
	for _, s := range ok {
		if NormalizeEventName(s) != s {
			t.Errorf("NormalizeEventName(%q) rejected a valid name", s)
		}
	}

	// 'pageview' is reserved: allowing it through the event API would let a site inflate its own
	// pageview count and make the headline number disagree with the pageview rollup.
	if NormalizeEventName("pageview") != "" {
		t.Error("'pageview' must be reserved — otherwise the event API can forge pageviews")
	}

	bad := []string{
		"", "   ",
		"has space", "has/slash", "has\ttab", "emoji🎉",
		"'; DROP TABLE analytics_event; --",
		strings.Repeat("x", maxEventNameLen+1),
	}
	for _, s := range bad {
		if got := NormalizeEventName(s); got != "" {
			t.Errorf("NormalizeEventName(%.30q) = %q, want rejected", s, got)
		}
	}
}

func TestSanitizeProps(t *testing.T) {
	got := SanitizeProps(map[string]any{
		"level":  float64(3),
		"score":  float64(1.5),
		"win":    true,
		"stage":  "final",
		"nested": map[string]any{"a": 1}, // dropped: not groupable
		"list":   []any{1, 2},            // dropped
		"empty":  "",
	})
	want := map[string]string{"level": "3", "score": "1.5", "win": "true", "stage": "final"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("props[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["nested"]; ok {
		t.Error("nested objects must be dropped — a JSON blob is not a groupable dashboard value")
	}
	if SanitizeProps(nil) != nil || SanitizeProps(map[string]any{}) != nil {
		t.Error("empty props should be nil")
	}
}

func TestSanitizeProps_BoundedAndDeterministic(t *testing.T) {
	in := map[string]any{}
	for i := 0; i < maxPropKeys*3; i++ {
		in[fmt.Sprintf("k%03d", i)] = "v"
	}
	first := SanitizeProps(in)
	if len(first) != maxPropKeys {
		t.Fatalf("key count = %d, want capped at %d", len(first), maxPropKeys)
	}
	// Truncation must be deterministic: map iteration order would otherwise store different
	// props for the identical payload on different requests.
	for i := 0; i < 5; i++ {
		again := SanitizeProps(in)
		if len(again) != len(first) {
			t.Fatalf("unstable key count: %d vs %d", len(again), len(first))
		}
		for k := range first {
			if _, ok := again[k]; !ok {
				t.Fatalf("unstable key selection: %q missing on rerun", k)
			}
		}
	}

	long := SanitizeProps(map[string]any{"k": strings.Repeat("é", maxPropValueLen)})
	if v := long["k"]; len(v) > maxPropValueLen || !utf8.ValidString(v) {
		t.Errorf("value not bounded on a rune boundary: %d bytes, valid=%v", len(v), utf8.ValidString(v))
	}
}

func TestSnippetJS_ExposesCustomEventAPI(t *testing.T) {
	for _, must := range []string{"window.mf", "window.mf.q"} {
		if !strings.Contains(snippetJS, must) {
			t.Errorf("snippet is missing %q", must)
		}
	}
	// The global must be assigned AFTER the pageview wiring, so a throw earlier cannot leave a
	// half-initialised mf() on the page.
	if strings.Index(snippetJS, "window.mf=") < strings.Index(snippetJS, "addEventListener('popstate'") {
		t.Error("window.mf is defined before the pageview wiring completes")
	}
}

// TestSnippetCacheValidator_DerivesFromContent pins the fix for a production incident: the snippet
// was served with a hand-maintained modtime constant, documented as "bump it with the content".
// It was not bumped across three snippet changes, so caches kept answering 304 and embedding sites
// silently ran an old tracker missing both UTM capture and the custom-event API. Traffic looked
// like it was being collected; parts of it simply were not.
//
// Anything a human must remember to update in lockstep with code eventually goes stale. These
// assert the validator is DERIVED, so the choice no longer exists.
func TestSnippetCacheValidator_DerivesFromContent(t *testing.T) {
	if snippetETag == "" || !strings.HasPrefix(snippetETag, `"`) {
		t.Fatalf("ETag must be a quoted strong validator, got %q", snippetETag)
	}

	// Two different bodies must never share a validator.
	sum := sha256.Sum256([]byte(snippetJS))
	want := `"` + hex.EncodeToString(sum[:16]) + `"`
	if snippetETag != want {
		t.Errorf("ETag is not the content hash: got %s want %s", snippetETag, want)
	}

	// The property that actually matters: DIFFERENT BODIES GET DIFFERENT VALIDATORS. Asserting the
	// validator is merely non-empty would pass for a constant, which is the bug.
	if etagFor("a") == etagFor("b") {
		t.Error("two different bodies produced the same ETag")
	}
	// A one-character change must move it — this is the granularity a real snippet edit has.
	if etagFor(snippetJS) == etagFor(snippetJS+" ") {
		t.Error("a modified snippet produced an unchanged ETag; caches would keep the old body")
	}

	// A hardcoded literal date in the source is exactly what caused the incident.
	src := mustReadFile(t, "snippet.go")
	if strings.Contains(src, "time.Date(2026") {
		t.Error("snippet.go contains a hardcoded modtime literal — it will go stale the next time " +
			"the snippet changes, exactly as it did before")
	}
	if !strings.Contains(src, "must-revalidate") {
		t.Error("the snippet response should require revalidation; a long opaque cache makes a " +
			"bad snippet unfixable for the duration of the TTL")
	}
}

func mustReadFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// TestServeSnippet_ConditionalRequests exercises the real HTTP contract. The derivation test
// proves the ETag tracks content; this proves the ETag is actually USED — a validator that never
// produces a 304 would turn every page load on every embedding site into a full re-download.
func TestServeSnippet_ConditionalRequests(t *testing.T) {
	h := &PublicHandler{}
	mux := chi.NewRouter()
	h.SnippetRoutes(mux)

	// First fetch: full body, with the validator and the short TTL.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first GET = %d, want 200", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the response")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=300") ||
		!strings.Contains(cc, "must-revalidate") {
		t.Errorf("Cache-Control = %q, want max-age=300 and must-revalidate", cc)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty snippet body")
	}

	// Revalidation with the same validator must be answered 304, not a fresh body.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/a.js", nil)
	req2.Header.Set("If-None-Match", etag)
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("revalidation = %d, want 304 (ETag not honoured; every page load would "+
			"re-download the snippet)", rec2.Code)
	}

	// A STALE validator must get the new body — this is the case that was broken in production:
	// the old snippet's validator kept matching after the content changed.
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/a.js", nil)
	req3.Header.Set("If-None-Match", `"0000000000000000000000000000000000000000"`)
	mux.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK || rec3.Body.Len() == 0 {
		t.Errorf("stale validator = %d (len %d), want 200 with the current snippet",
			rec3.Code, rec3.Body.Len())
	}

	// No Last-Modified. A date implies an ordering that content does not have, and a validator
	// derived from a hash can move BACKWARDS when the snippet changes — after which ServeContent
	// answers 304 to any If-Modified-Since at or after it, pinning the stale body. The ETag is the
	// only validator, precisely so there is no ordering to get wrong.
	if lm := rec.Header().Get("Last-Modified"); lm != "" {
		t.Errorf("Last-Modified = %q, want absent: a date-shaped validator can regress across "+
			"snippet changes and re-create the staleness bug", lm)
	}

	// The case that matters for the object currently stuck in Cloudflare: it was cached before this
	// handler set an ETag, so it can only revalidate with If-Modified-Since. That must yield the
	// CURRENT body, never a 304.
	rec4 := httptest.NewRecorder()
	req4 := httptest.NewRequest(http.MethodGet, "/a.js", nil)
	req4.Header.Set("If-Modified-Since", time.Now().Add(24*time.Hour).UTC().Format(http.TimeFormat))
	mux.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK || rec4.Body.Len() == 0 {
		t.Errorf("If-Modified-Since (far future) = %d (len %d), want 200 — a client holding an old "+
			"copy with only a date must still receive the new snippet", rec4.Code, rec4.Body.Len())
	}
}
