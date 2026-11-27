// Finding: PORTAL-HOST-ISOLATION — a custom portal hostname must never reach operator routes.
//
// No build tag: source-level pins that run in `make test` and `make sec-test`.
package security_regression

import (
	"regexp"
	"strings"
	"testing"
)

func TestPin_PortalHostRoutingWrapsServer(t *testing.T) {
	main := mustRead(t, "../../cmd/manyforge/main.go")
	if !strings.Contains(main, "portaldomain.HostRouting(") {
		t.Error("cmd/manyforge/main.go must wrap the server handler in portaldomain.HostRouting")
	}
}

// The allowlist must stay an allowlist: no operator path may be named in the router.
func TestPin_PortalHostRoutingNamesNoOperatorPaths(t *testing.T) {
	src := mustRead(t, "../../internal/portaldomain/hostrouting.go")
	for _, forbidden := range []string{"/api/v1/auth", "/api/passkeys", "/metrics", "/cli"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("hostrouting.go must not mention %q", forbidden)
		}
	}
}

func TestPin_PortalHostIgnoresForwardingHeaders(t *testing.T) {
	src := mustRead(t, "../../internal/portaldomain/hostrouting.go")
	if strings.Contains(strings.ToLower(src), "x-forwarded") || strings.Contains(src, "Header.Get") {
		t.Error("hostrouting.go must decide the hostname from r.Host only")
	}
}

func TestPin_PasskeyCookiesAreHostBound(t *testing.T) {
	src := mustRead(t, "../../internal/account/passkey_handler.go")
	if !strings.Contains(src, "__Host-") {
		t.Error("passkey cookies must use the __Host- prefix")
	}
	if strings.Contains(src, "Domain:") {
		t.Error("passkey cookies must not set a Domain attribute")
	}
}

// Principal-less SECURITY DEFINER functions must stay locked down: definer rights, a
// pinned search_path, no PUBLIC execute, and execute granted only to the app role.
func TestPin_PortalDomainDefinersLockedDown(t *testing.T) {
	cases := []struct {
		file string
		fns  []string
	}{
		{"../../migrations/0155_portal_domains.up.sql", []string{"portal_domain_resolve", "portal_domain_primary"}},
		{"../../migrations/0156_portal_domain_takeover.up.sql", []string{"portal_domain_takeover_stale"}},
	}
	for _, c := range cases {
		src := mustRead(t, c.file)
		for _, fn := range c.fns {
			def := regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+` + fn + `\s*\(.*?\$\$`).FindString(src)
			if def == "" {
				t.Errorf("%s: no CREATE FUNCTION %s", c.file, fn)
				continue
			}
			if !regexp.MustCompile(`(?i)SECURITY\s+DEFINER`).MatchString(def) {
				t.Errorf("%s: %s is not SECURITY DEFINER", c.file, fn)
			}
			if !regexp.MustCompile(`(?i)SET\s+search_path\s*=\s*public\s*,\s*pg_temp`).MatchString(def) {
				t.Errorf("%s: %s lacks a pinned search_path", c.file, fn)
			}
			if !regexp.MustCompile(`(?is)REVOKE\s+ALL\s+ON\s+FUNCTION\s+` + fn + `\s*\([^)]*\)\s+FROM\s+PUBLIC`).MatchString(src) {
				t.Errorf("%s: %s is not revoked from PUBLIC", c.file, fn)
			}
			if !regexp.MustCompile(`(?is)GRANT\s+EXECUTE\s+ON\s+FUNCTION\s+` + fn + `\s*\([^)]*\)\s+TO\s+manyforge_app`).MatchString(src) {
				t.Errorf("%s: %s is not granted to manyforge_app", c.file, fn)
			}
		}
	}
}
