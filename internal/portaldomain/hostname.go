// Package portaldomain manages business-scoped custom hostnames for the public
// customer portal: hostname hygiene, DNS TXT ownership verification and the
// global exclusivity of verified hostnames.
package portaldomain

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// InstanceHost returns the normalised host of the instance's public base URL. An
// empty URL yields "", nil (host routing off). A URL that does not parse, has no
// host, or has a scheme other than http or https is an error, so a schemeless
// value such as "app.example.test" is never silently treated as "no host".
func InstanceHost(baseURL string) (string, error) {
	if baseURL == "" {
		return "", nil
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse public base URL: %w", err)
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return "", errors.New("public base URL must use http or https")
	}
	h := NormalizeHost(u.Host)
	if h == "" {
		return "", errors.New("public base URL has no host")
	}
	return h, nil
}

// NormalizeHost lower-cases a Host value and strips its port and one trailing dot.
func NormalizeHost(hostport string) string {
	h := strings.TrimSpace(hostport)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	h = strings.TrimSuffix(h, ".")
	return strings.ToLower(h)
}

// ValidHostname reports whether h is an ASCII FQDN usable as a portal hostname: at
// least two labels of 1-63 letters, digits or hyphens (no leading/trailing hyphen),
// at most 253 characters in total, an alphabetic TLD of 2+ characters, and neither a
// wildcard nor an IP literal.
func ValidHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return false
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if len(l) < 1 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				return false
			}
		}
	}
	tld := labels[len(labels)-1]
	if len(tld) < 2 {
		return false
	}
	for i := 0; i < len(tld); i++ {
		c := tld[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// IsInternalHost reports whether h names the platform's own network rather than a
// public customer hostname: an IP literal, localhost, a cluster-internal name, or
// any non-empty single-label name (no dot), such as a bare in-cluster service name.
func IsInternalHost(h string) bool {
	h = strings.ToLower(h)
	if h != "" && !strings.Contains(h, ".") {
		return true
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	if h == "localhost" {
		return true
	}
	for _, suf := range []string{".localhost", ".svc", ".svc.cluster.local"} {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}
