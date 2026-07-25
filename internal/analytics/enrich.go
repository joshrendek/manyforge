package analytics

import (
	"net"
	"net/url"
	"strings"
)

// Enrichment turns the two pieces of untrusted, identifying request data we hold transiently — the
// User-Agent and the IP — into low-cardinality buckets that are safe to store.
//
// The distinction is the whole privacy argument. A full User-Agent string is a fingerprint: enough
// entropy to re-identify a visitor across days even without a cookie. "Safari" is not. A raw IP
// identifies a household; "US" does not. So the rule for anything added here is: if a value could
// meaningfully narrow a population to a person, it does not belong in a column.

// maxDimensionLen bounds a stored dimension value. These become GROUP BY keys, so an unbounded
// attacker-supplied string would bloat the rollup.
const maxDimensionLen = 128

// UTM holds the campaign attribution a tenant's marketing needs.
type UTM struct {
	Source   string
	Medium   string
	Campaign string
}

// ParseUTM extracts ONLY the three utm_* keys from a query string.
//
// It is deliberately an allowlist rather than "store the query string and pick fields out later":
// query strings routinely carry session tokens, password-reset codes, and email addresses, and the
// moment the raw string is persisted it is a PII store. The snippet also filters client-side; this
// is the server-side guarantee, since the endpoint is public and a caller can send anything.
func ParseUTM(rawQuery string) UTM {
	rawQuery = strings.TrimPrefix(rawQuery, "?")
	if rawQuery == "" {
		return UTM{}
	}
	v, err := url.ParseQuery(rawQuery)
	if err != nil {
		return UTM{}
	}
	return UTM{
		Source:   clampDimension(v.Get("utm_source")),
		Medium:   clampDimension(v.Get("utm_medium")),
		Campaign: clampDimension(v.Get("utm_campaign")),
	}
}

func clampDimension(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Control characters would corrupt logs and dashboards; strip rather than reject so a single
	// odd campaign tag does not discard an otherwise good pageview.
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > maxDimensionLen {
		s = s[:maxDimensionLen]
	}
	return s
}

// DeviceType classifies a User-Agent into exactly three buckets.
//
// Three is a deliberate ceiling, not laziness. Finer classification ("iPhone 15 Pro", an exact OS
// build) is precisely the information that turns a device string into a fingerprint, and the
// question a dashboard actually answers is "should I care about mobile layout?".
func DeviceType(ua string) string {
	if ua == "" {
		return ""
	}
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "ipad"),
		strings.Contains(l, "tablet"),
		strings.Contains(l, "kindle"),
		strings.Contains(l, "playbook"),
		// Android WITHOUT "mobile" is the conventional tablet signal.
		strings.Contains(l, "android") && !strings.Contains(l, "mobile"):
		return "tablet"
	case strings.Contains(l, "mobi"),
		strings.Contains(l, "iphone"),
		strings.Contains(l, "ipod"),
		strings.Contains(l, "android"),
		strings.Contains(l, "windows phone"):
		return "mobile"
	default:
		return "desktop"
	}
}

// browserRules are ordered because User-Agent strings lie by design: Edge claims to be Chrome,
// Chrome claims to be Safari. The first match wins, so the most-specific impostor must be checked
// before the identity it imitates.
var browserRules = []struct {
	needle string
	name   string
}{
	{"edg/", "Edge"},  // Edge says "Chrome" and "Safari" too
	{"opr/", "Opera"}, // Opera says "Chrome"
	{"samsungbrowser", "Samsung Internet"},
	{"firefox", "Firefox"},
	{"chrome", "Chrome"}, // must come after Edge/Opera
	{"safari", "Safari"}, // must come last: Chrome and Edge both claim it
}

// Browser reduces a User-Agent to a coarse family name, or "" when unrecognised.
func Browser(ua string) string {
	if ua == "" {
		return ""
	}
	l := strings.ToLower(ua)
	for _, r := range browserRules {
		if strings.Contains(l, r.needle) {
			return r.name
		}
	}
	return "Other"
}

// CountryResolver maps an IP to an ISO 3166-1 alpha-2 code. A nil resolver, or one that cannot
// place an address, yields "" — the country column is then simply absent rather than wrong.
type CountryResolver interface {
	Country(ip net.IP) string
}

// ResolveCountry is nil-safe and rejects addresses that cannot carry meaningful geography, so a
// local or private address is reported as unknown rather than as whatever the database guesses.
func ResolveCountry(r CountryResolver, ipStr string) string {
	if r == nil || ipStr == "" {
		return ""
	}
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return ""
	}
	c := r.Country(ip)
	if len(c) != 2 {
		return ""
	}
	return strings.ToUpper(c)
}
