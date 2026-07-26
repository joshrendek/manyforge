package main

import (
	"net"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/config"
)

func TestAnalyticsCountryTrustProductionWiring(t *testing.T) {
	_, trustedProxy, err := net.ParseCIDR("10.244.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	trusted := []*net.IPNet{trustedProxy}

	for _, tc := range []struct {
		name         string
		env          string
		trustedProxy string
		want         bool
	}{
		{name: "enabled", env: "true", trustedProxy: "10.244.0.0/16", want: true},
		{name: "disabled by default", env: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MANYFORGE_TRUST_CF_IPCOUNTRY", tc.env)
			t.Setenv("MANYFORGE_TRUSTED_PROXY_CIDR", tc.trustedProxy)
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("config.Load: %v", err)
			}

			handler := newAnalyticsPublicHandler(nil, nil, nil, trusted, cfg)
			if handler.TrustCloudflareCountryHeader != tc.want {
				t.Fatalf("TrustCloudflareCountryHeader = %t, want %t",
					handler.TrustCloudflareCountryHeader, tc.want)
			}
			if len(handler.TrustedProxies) != 1 || handler.TrustedProxies[0] != trustedProxy {
				t.Fatalf("TrustedProxies = %v, want production proxy set preserved", handler.TrustedProxies)
			}
		})
	}
}
