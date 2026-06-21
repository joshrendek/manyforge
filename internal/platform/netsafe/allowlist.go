package netsafe

import (
	"net"
	"strings"
)

// HostAllowlist is an exact-match egress allowlist of bare hostnames. It is the
// single source of truth shared by the CONNECT egress proxy (which enforces it,
// cmd/mf-egress-proxy) and the code-review service (which validates against it
// before launching a sandbox, internal/agents/coding). Sharing one matcher keeps
// the validator and the enforcer from drifting — a drift would either silently
// block a permitted provider or, worse, admit one the proxy rejects (manyforge-0qj).
//
// A nil or empty allowlist denies everything (fail-closed).
type HostAllowlist map[string]bool

// ParseHostAllowlist builds a HostAllowlist from a comma-separated list of
// hostnames, trimming surrounding whitespace and dropping empty entries.
func ParseHostAllowlist(csv string) HostAllowlist {
	set := HostAllowlist{}
	for h := range strings.SplitSeq(csv, ",") {
		if h = strings.TrimSpace(h); h != "" {
			set[h] = true
		}
	}
	return set
}

// Allows reports whether hostport is permitted. hostport may be a bare host
// ("api.anthropic.com") or a host:port ("api.anthropic.com:443"); the bare host
// is what must appear in the allowlist. The full host:port form is also accepted
// so a literal "host:port" allowlist entry still matches.
func (a HostAllowlist) Allows(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return a[host] || a[hostport]
}
