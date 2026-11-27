package main

import (
	"strings"
	"testing"
)

func TestResolveInstanceHost(t *testing.T) {
	if h, err := resolveInstanceHost(""); h != "" || err != nil {
		t.Fatalf("empty: got %q, %v; want routing off", h, err)
	}
	if h, err := resolveInstanceHost("https://app.example.test:8443/"); h != "app.example.test" || err != nil {
		t.Fatalf("valid: got %q, %v", h, err)
	}
	for _, bad := range []string{"https://:8443", "https://secret:pw@app.example.test", "app.example.test", "https://bücher.example"} {
		h, err := resolveInstanceHost(bad)
		if err == nil || h != "" {
			t.Errorf("%q: got %q, %v; want an error", bad, h, err)
			continue
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "pw") {
			t.Errorf("%q: error leaks userinfo: %v", bad, err)
		}
	}
}
