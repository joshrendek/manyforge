package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestPublicIngestOriginsDisabledByDefault(t *testing.T) {
	t.Setenv("MANYFORGE_PUBLIC_INGEST_ALLOWED_ORIGINS", "")
	t.Setenv("MANYFORGE_PUBLIC_BASE_URL", "")
	t.Setenv("MANYFORGE_MAILING_MASTER_KEY", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PublicIngestAllowedOrigins) != 0 {
		t.Fatalf("CORS unexpectedly enabled: %v", cfg.PublicIngestAllowedOrigins)
	}
}

func TestPublicIngestOriginsNormalizeAndDeduplicate(t *testing.T) {
	t.Setenv("MANYFORGE_PUBLIC_INGEST_ALLOWED_ORIGINS", " https://CUSTOMER.example:443/ ,http://localhost:4310,https://customer.example ")
	t.Setenv("MANYFORGE_PUBLIC_BASE_URL", " https://INSTANCE.example:443/ ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://customer.example", "http://localhost:4310"}
	if !reflect.DeepEqual(cfg.PublicIngestAllowedOrigins, want) || cfg.PublicBaseURL != "https://instance.example" {
		t.Fatalf("normalized origins=%v instance=%q", cfg.PublicIngestAllowedOrigins, cfg.PublicBaseURL)
	}
}

func TestPublicIngestOriginsRejectMalformedNonemptyConfiguration(t *testing.T) {
	for _, raw := range []string{
		" ", ",", "https://customer.example,", ",https://customer.example",
		"*", "null", "https://*.example", "https://customer.example/page",
		"https://customer.example?query=yes", "https://customer.example#fragment",
		"https://user:password@customer.example", "http://customer.example",
		"not-an-origin", "https://customer.example:0",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("MANYFORGE_PUBLIC_INGEST_ALLOWED_ORIGINS", raw)
			t.Setenv("MANYFORGE_PUBLIC_BASE_URL", "https://instance.example")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "MANYFORGE_PUBLIC_INGEST_ALLOWED_ORIGINS") {
				t.Fatalf("malformed CORS configuration must fail startup: %v", err)
			}
		})
	}
}

func TestPublicIngestOriginsRequireValidInstanceOrigin(t *testing.T) {
	for _, raw := range []string{"", "null", "*", "https://instance.example/path", "https://instance.example//", "https://instance.example?query=yes", "https://user@instance.example"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("MANYFORGE_PUBLIC_INGEST_ALLOWED_ORIGINS", "https://customer.example")
			t.Setenv("MANYFORGE_PUBLIC_BASE_URL", raw)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "MANYFORGE_PUBLIC_BASE_URL") {
				t.Fatalf("enabled CORS without valid instance origin must fail startup: %v", err)
			}
		})
	}
}
