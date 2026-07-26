package main

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/config"
)

func TestAnalyticsCountryTrustProductionWiring(t *testing.T) {
	for _, tc := range []struct {
		name         string
		env          string
		trustedProxy string
		want         bool
	}{
		{name: "enabled", env: "true", trustedProxy: "10.244.0.0/16", want: true},
		{name: "disabled by default", env: "", trustedProxy: "10.244.0.0/16", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MANYFORGE_TRUST_CF_IPCOUNTRY", tc.env)
			t.Setenv("MANYFORGE_TRUSTED_PROXY_CIDR", tc.trustedProxy)
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			trusted := parseTrustedCIDRs(cfg.TrustedProxyCIDR, logger)
			handler := newAnalyticsPublicHandler(nil, nil, nil, trusted, cfg)
			if handler.TrustCloudflareCountryHeader != tc.want {
				t.Fatalf("TrustCloudflareCountryHeader = %t, want %t",
					handler.TrustCloudflareCountryHeader, tc.want)
			}
			if len(handler.TrustedProxies) != 1 {
				t.Fatalf("TrustedProxies = %v, want production proxy set preserved", handler.TrustedProxies)
			}

			matching := &http.Request{RemoteAddr: "10.244.7.9:5000", Header: http.Header{"CF-IPCountry": {"US"}}}
			if _, got := handler.ResolveClient(matching); got != tc.want {
				t.Fatalf("matching proxy country trust = %t, want %t", got, tc.want)
			}
			untrusted := &http.Request{RemoteAddr: "203.0.113.9:5000", Header: http.Header{"CF-IPCountry": {"US"}}}
			if _, got := handler.ResolveClient(untrusted); got {
				t.Fatal("untrusted direct peer was allowed to assert CF-IPCountry")
			}
		})
	}
}
