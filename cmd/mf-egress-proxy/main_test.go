package main

import (
	"testing"

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
