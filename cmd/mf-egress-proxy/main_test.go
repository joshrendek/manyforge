package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// The proxy enforces egress via netsafe.HostAllowlist (the same matcher the
// code-review service validates against). This pins the proxy's CONNECT-host
// matching contract; the exhaustive parse/match cases live in netsafe's tests.
func TestProxyAllowMatching(t *testing.T) {
	tests := []struct {
		name  string
		set   netsafe.HostAllowlist
		query string
		want  bool
	}{
		{
			name:  "host-only allowlist matches host:port",
			set:   netsafe.HostAllowlist{"api.anthropic.com": true},
			query: "api.anthropic.com:443",
			want:  true,
		},
		{
			name:  "hostport allowlist matches exact hostport",
			set:   netsafe.HostAllowlist{"api.anthropic.com:443": true},
			query: "api.anthropic.com:443",
			want:  true,
		},
		{
			name:  "non-allowlisted host denied",
			set:   netsafe.HostAllowlist{"api.anthropic.com": true},
			query: "evil.example.com:443",
			want:  false,
		},
		{
			name:  "empty allowlist denies everything",
			set:   netsafe.HostAllowlist{},
			query: "api.anthropic.com:443",
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Allows(tt.query); got != tt.want {
				t.Errorf("Allows(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestProxyForwardsAllowlistedPlainHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://") // 127.0.0.1:PORT

	proxy := httptest.NewServer(proxyHandler(netsafe.ParseHostAllowlist(host)))
	defer proxy.Close()

	// A client that sends every request through the proxy (plain-HTTP ⇒ absolute-form, not CONNECT).
	pu, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}

	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("get through proxy: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "upstream-ok" {
		t.Fatalf("want 200/upstream-ok, got %d/%q", resp.StatusCode, b)
	}
}

func TestProxyRejectsNonAllowlistedPlainHTTP(t *testing.T) {
	proxy := httptest.NewServer(proxyHandler(netsafe.ParseHostAllowlist("api.anthropic.com")))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := client.Get("http://198.51.100.7:9/x") // not allowlisted, never dialed
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
}

func TestProxyStreamsChunksIncrementally(t *testing.T) {
	// Upstream writes chunk 1 + flush, then BLOCKS until the client has read it —
	// proving the proxy delivers chunk 1 before the upstream sends chunk 2 (i.e.
	// flushWriter forwards incrementally instead of buffering the whole body).
	// A buffering proxy would deadlock the upstream (client can't read chunk 1
	// until the body is done) → the 2s guard fires → test fails.
	gotFirst := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter is not a Flusher")
			return
		}
		_, _ = io.WriteString(w, "chunk1\n")
		fl.Flush()
		select {
		case <-gotFirst:
		case <-time.After(2 * time.Second):
			t.Error("client did not read first chunk before upstream timeout (proxy buffered)")
		}
		_, _ = io.WriteString(w, "chunk2\n")
	}))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")

	proxy := httptest.NewServer(proxyHandler(netsafe.ParseHostAllowlist(host)))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}

	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("get through proxy: %v", err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line1, err := br.ReadString('\n')
	if err != nil || line1 != "chunk1\n" {
		t.Fatalf("first chunk: got %q err %v (proxy did not stream incrementally)", line1, err)
	}
	close(gotFirst) // release the upstream to send chunk 2
	line2, err := br.ReadString('\n')
	if err != nil || line2 != "chunk2\n" {
		t.Fatalf("second chunk: got %q err %v", line2, err)
	}
}
