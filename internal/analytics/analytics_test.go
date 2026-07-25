package analytics

import (
	"net"
	"strings"
	"testing"
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

// stubGeo answers every lookup with a fixed code, so ResolveCountry's filtering is what is tested.
type stubGeo struct{ code string }

func (s stubGeo) Country(net.IP) string { return s.code }

func TestResolveCountry(t *testing.T) {
	g := stubGeo{"us"}
	if got := ResolveCountry(g, "203.0.113.9"); got != "US" {
		t.Errorf("public IP: got %q, want US (uppercased)", got)
	}
	// A nil resolver is the default deployment state and must be safe.
	if got := ResolveCountry(nil, "203.0.113.9"); got != "" {
		t.Errorf("nil resolver: got %q, want empty", got)
	}
	// Addresses that cannot carry meaningful geography are reported as unknown rather than as
	// whatever the database happens to say.
	for _, ip := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "169.254.1.1", "0.0.0.0", "::1", "not-an-ip", ""} {
		if got := ResolveCountry(g, ip); got != "" {
			t.Errorf("ResolveCountry(%q) = %q, want empty", ip, got)
		}
	}
	// A malformed code from the database is discarded rather than stored.
	if got := ResolveCountry(stubGeo{"UNITED STATES"}, "203.0.113.9"); got != "" {
		t.Errorf("non-alpha-2 code should be dropped, got %q", got)
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
