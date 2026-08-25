package weborigin

import "testing"

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"https", "https://Example.COM/", "https://example.com"},
		{"default https port", "HTTPS://Example.COM:443", "https://example.com"},
		{"non-default port", "https://example.com:8443", "https://example.com:8443"},
		{"leading-zero port", "https://example.com:00001", "https://example.com:1"},
		{"localhost", "http://LOCALHOST:4300/", "http://localhost:4300"},
		{"loopback ipv4", "http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"loopback ipv6", "http://[::1]:8080", "http://[::1]:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.raw)
			if err != nil || got != tc.want {
				t.Fatalf("Normalize(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
			}
		})
	}
}

func TestNormalizeRejectsNonOrigins(t *testing.T) {
	for _, raw := range []string{
		"", "null", "example.com", "http://example.com", "https://*.example.com",
		"https://user@example.com", "https://example.com/path", "https://example.com/?q=x",
		"https://example.com/#fragment", "https://example.com,https://evil.test",
		"https://-bad.example", "https://bad_.example", "https://example.com:0",
		"https://example.com:65536", "https://example.com:",
	} {
		t.Run(raw, func(t *testing.T) {
			if got, err := Normalize(raw); err == nil {
				t.Fatalf("Normalize(%q) = %q, want error", raw, got)
			}
		})
	}
}

func TestFromHeaderRequiresOneValue(t *testing.T) {
	for _, values := range [][]string{
		nil,
		{},
		{"null"},
		{"not-a-url"},
		{"https://example.com", "https://evil.test"},
		{"https://example.com, https://evil.test"},
	} {
		if got, err := FromHeader(values); err == nil {
			t.Fatalf("FromHeader(%q) = %q, want error", values, got)
		}
	}
	if got, err := FromHeader([]string{"https://EXAMPLE.com:443/"}); err != nil || got != "https://example.com" {
		t.Fatalf("FromHeader valid = %q, %v", got, err)
	}
}
