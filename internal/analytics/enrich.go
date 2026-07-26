package analytics

import (
	"math"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
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
	// This is public input, so it may not be valid UTF-8 at all. Postgres rejects invalid UTF-8
	// outright, and because the collect endpoint answers 204 unconditionally, that rejection would
	// silently DISCARD the whole pageview — a data-loss bug with no visible symptom.
	s = strings.ToValidUTF8(s, "")

	// Control characters would corrupt logs and dashboards; strip rather than reject so one odd
	// campaign tag does not discard an otherwise good pageview.
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)

	// Truncate on a RUNE boundary. Slicing bytes can cut a multibyte character in half and
	// re-introduce exactly the invalid-UTF-8 rejection guarded against above.
	if len(s) > maxDimensionLen {
		s = s[:maxDimensionLen]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
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

// ---------------------------------------------------------------------------
// Custom events
// ---------------------------------------------------------------------------

// reservedEventName is the implicit name for an automatic pageview. A caller may not send it
// explicitly, or a site could inflate its own pageview count through the event API and make the
// headline number disagree with the pageview rollup.
const reservedEventName = "pageview"

const (
	maxEventNameLen = 64
	maxPropKeys     = 12
	maxPropKeyLen   = 32
	maxPropValueLen = 128
)

// eventNameOK bounds an event name to a conservative identifier shape.
//
// Event names become GROUP BY keys in the rollup and labels on a dashboard, so they are treated
// like identifiers rather than free text: unbounded or control-laden names would corrupt both.
func eventNameOK(s string) bool {
	if s == "" || len(s) > maxEventNameLen || s == reservedEventName {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

// NormalizeEventName returns the event name to store, or "" if the caller sent nothing usable.
// An invalid name is rejected rather than coerced: silently renaming someone's event would be
// worse than dropping it, because the dashboard would show a metric they never emitted.
func NormalizeEventName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !eventNameOK(s) {
		return ""
	}
	return s
}

// SanitizeProps bounds custom event properties.
//
// Props are attacker-supplied and land in a jsonb column, so both the key count and the value
// sizes are capped. Values are flattened to strings: nested objects invite unbounded nesting and
// give a dashboard nothing it can group by, and this is the same reason the query string is not
// stored wholesale — a free-form bag is where PII ends up.
func SanitizeProps(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	// Sorted so truncation past the key cap is deterministic rather than map-order dependent —
	// otherwise the same payload could store different props on different requests.
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if len(out) >= maxPropKeys {
			break
		}
		ck := clampDimension(k)
		if ck == "" || len(ck) > maxPropKeyLen {
			continue
		}
		var v string
		switch t := in[k].(type) {
		case string:
			v = t
		case bool:
			v = strconv.FormatBool(t)
		case float64:
			// JSON numbers decode as float64; render integers without a trailing .0
			if t == math.Trunc(t) && math.Abs(t) < 1e15 {
				v = strconv.FormatInt(int64(t), 10)
			} else {
				v = strconv.FormatFloat(t, 'f', -1, 64)
			}
		default:
			// Nested objects and arrays are dropped rather than stringified: a JSON blob in a
			// dashboard cell is not a groupable value.
			continue
		}
		v = clampDimension(v)
		if v == "" {
			continue
		}
		if len(v) > maxPropValueLen {
			v = v[:maxPropValueLen]
			for len(v) > 0 && !utf8.ValidString(v) {
				v = v[:len(v)-1]
			}
		}
		out[ck] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
