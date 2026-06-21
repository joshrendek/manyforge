package main

import "testing"

func TestAllowed(t *testing.T) {
	set := map[string]bool{"api.anthropic.com": true}
	if !allowed(set, "api.anthropic.com:443") {
		t.Fatal("should allow allowlisted host with port")
	}
	if allowed(set, "evil.example.com:443") {
		t.Fatal("should deny non-allowlisted host")
	}
}
