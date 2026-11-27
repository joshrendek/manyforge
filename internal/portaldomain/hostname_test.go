package portaldomain

import (
	"strings"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"Portal.Example.COM:8443": "portal.example.com",
		"portal.example.com.":     "portal.example.com",
		"[::1]:80":                "::1",
		"portal.example.com":      "portal.example.com",
	}
	for in, want := range cases {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidHostname(t *testing.T) {
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 58) + ".com"
	if len(long) != 254 {
		t.Fatalf("fixture length %d", len(long))
	}
	valid := []string{"portal.example.com", "a-b.example.co"}
	invalid := []string{"example", "*.example.com", "10.0.0.1", "::1", "-a.example.com", "a..example.com",
		"portal.example.c0m", long, "pörtal.example.com", "portal.example.com:443", "",
		"a-.example.com", "portal.example.c"}
	for _, h := range valid {
		if !ValidHostname(h) {
			t.Errorf("ValidHostname(%q) = false, want true", h)
		}
	}
	for _, h := range invalid {
		if ValidHostname(h) {
			t.Errorf("ValidHostname(%q) = true, want false", h)
		}
	}
}

func TestIsInternalHost(t *testing.T) {
	for _, h := range []string{"127.0.0.1", "localhost", "app.localhost", "api.ns.svc", "api.ns.svc.cluster.local", "manyforge", "api"} {
		if !IsInternalHost(h) {
			t.Errorf("IsInternalHost(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"", "portal.example.com", "svc.example.com"} {
		if IsInternalHost(h) {
			t.Errorf("IsInternalHost(%q) = true, want false", h)
		}
	}
}

func TestInstanceHost(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"https://app.example.test", "app.example.test", false},
		{"https://app.example.test:8443", "app.example.test", false},
		{"https://app.example.test/", "app.example.test", false},
		{"HTTPS://APP.Example.Test", "app.example.test", false},
		{"http://app.example.test", "app.example.test", false},
		{"app.example.test", "", true},
		{"ftp://app.example.test", "", true},
		{"https://", "", true},
		{"https:///path", "", true},
	}
	for _, c := range cases {
		got, err := InstanceHost(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("InstanceHost(%q) = %q, %v; want %q, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}
