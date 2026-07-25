package analytics

import (
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
