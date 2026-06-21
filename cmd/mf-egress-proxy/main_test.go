package main

import "testing"

func TestAllowed(t *testing.T) {
	tests := []struct {
		name    string
		set     map[string]bool
		query   string
		want    bool
	}{
		{
			name:  "host-only allowlist matches host:port",
			set:   map[string]bool{"api.anthropic.com": true},
			query: "api.anthropic.com:443",
			want:  true,
		},
		{
			name:  "hostport allowlist matches exact hostport",
			set:   map[string]bool{"api.anthropic.com:443": true},
			query: "api.anthropic.com:443",
			want:  true,
		},
		{
			name:  "non-allowlisted host denied",
			set:   map[string]bool{"api.anthropic.com": true},
			query: "evil.example.com:443",
			want:  false,
		},
		{
			name:  "empty allowlist denies everything",
			set:   map[string]bool{},
			query: "api.anthropic.com:443",
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allowed(tt.set, tt.query); got != tt.want {
				t.Errorf("allowed(%v, %q) = %v, want %v", tt.set, tt.query, got, tt.want)
			}
		})
	}
}
