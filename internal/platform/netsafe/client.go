// Package netsafe provides an SSRF-guarded HTTP client for any outbound request
// influenced by user or agent input (Constitution Principle II). It resolves the
// target host before dialing and refuses private, loopback, link-local, and
// cloud-metadata addresses — required even behind a host allowlist, because DNS
// can rebind.
package netsafe

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

var metadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"), // AWS/GCP/Azure IMDS
	net.ParseIP("fd00:ec2::254"),   // AWS IMDS IPv6
}

// blockedWith reports whether ip must be refused. allowLoopback permits 127/8 + ::1;
// allowPrivate permits RFC1918 + IPv6 ULA (fc00::/7). Cloud-metadata and link-local
// addresses are refused unconditionally — a trusted credential must never reach IMDS.
func blockedWith(ip net.IP, allowLoopback, allowPrivate bool) bool {
	if ip == nil {
		return true
	}
	// (1) Metadata IPs: blocked before any flag. fd00:ec2::254 is itself an fc00::/7
	// ULA, so this MUST precede the IsPrivate() gate or allowPrivate would leak IMDS.
	for _, m := range metadataIPs {
		if ip.Equal(m) {
			return true
		}
	}
	// (2) Link-local (incl. 169.254.169.254 IMDS-v4), multicast, unspecified: always blocked.
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// (3) Loopback: permitted only when explicitly trusted.
	if ip.IsLoopback() {
		return !allowLoopback
	}
	// (4) RFC1918 + IPv6 ULA: permitted only when explicitly trusted.
	if ip.IsPrivate() {
		return !allowPrivate
	}
	return false
}

// Blocked reports whether ip is a destination outbound requests must refuse
// (loopback + private blocked — the default, locked-secure posture).
func Blocked(ip net.IP) bool { return blockedWith(ip, false, false) }

// IsBlocked reports whether ip must be refused under o. Exposed so a caller can
// pre-validate a LITERAL base_url host with the EXACT dialer policy (see the
// credential service's create-time guard) rather than reimplementing it.
func IsBlocked(ip net.IP, o Options) bool { return blockedWith(ip, o.AllowLoopback, o.AllowPrivate) }

// Options configures a guarded client.
type Options struct {
	AllowLoopback bool // permits 127/8 + ::1 (dev MCP / self-host)
	AllowPrivate  bool // permits RFC1918 + IPv6 ULA; metadata stays blocked
}

// vettedDialAddrs checks every resolved IP against the dialer policy and returns EVERY
// address as a literal host:port, in resolution order.
//
// Two properties, both load-bearing:
//
//   - Fail closed if ANY resolved address is blocked. A name that resolves to a mix of public
//     and private addresses must not be dialable at all — that is the DNS-rebinding defense.
//   - Return literal IPs, never the hostname, so the caller dials an address we vetted and
//     cannot re-resolve to a different one between the check and the dial (TOCTOU).
//
// It returns all of them rather than just ips[0] so the caller can fall back when one address
// is unreachable. A multi-homed CDN host (router.huggingface.co has four A records) otherwise
// fails the whole request whenever its first address blips, which the stdlib dialer would have
// ridden out by trying the rest (manyforge-bhx). Every returned address is already vetted, so
// falling back weakens nothing.
//
// An empty ips slice is an error: LookupIPAddr normally errors when it resolves nothing, but
// that guarantee is soft — an empty, non-error result would otherwise dial nowhere.
func vettedDialAddrs(ips []net.IPAddr, host, port string, o Options) ([]string, error) {
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses resolved for host %s", host)
	}
	for _, ip := range ips {
		if blockedWith(ip.IP, o.AllowLoopback, o.AllowPrivate) {
			return nil, fmt.Errorf("blocked address %s for host %s", ip.IP, host)
		}
	}
	addrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, net.JoinHostPort(ip.IP.String(), port))
	}
	return addrs, nil
}

// ScreenedDialContext returns a DialContext that resolves the host and refuses any
// resolved IP blocked under o (metadata/link-local always; loopback/private per o).
// This is the resolved-IP screen shared by NewClientWithOptions and any other caller
// that dials a caller-influenced host directly (e.g. the egress proxy, manyforge-9er) —
// an allowlisted hostname is not enough on its own, because DNS can rebind the same
// name to a blocked address between the allowlist check and the dial.
// lookupIPAddr is the resolver ScreenedDialContext screens. A package var, not a parameter,
// so tests can exercise the real dial-and-fallback path without a live DNS name that happens
// to resolve to both a dead and a live address.
var lookupIPAddr = net.DefaultResolver.LookupIPAddr

func ScreenedDialContext(o Options) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := lookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		targets, err := vettedDialAddrs(ips, host, port, o)
		if err != nil {
			return nil, err
		}
		// Try each vetted address in order, as the stdlib dialer would. Every target is a
		// literal IP that already passed the policy screen, so this adds availability without
		// widening what we are willing to dial. Stop early once the caller's deadline is gone.
		var lastErr error
		for _, target := range targets {
			conn, dialErr := dialer.DialContext(ctx, network, target)
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
			if ctx.Err() != nil {
				break
			}
		}
		return nil, lastErr
	}
}

// NewClientWithOptions builds a guarded client configured by o. See Options for the
// available trust flags; the zero-value Options is the fully locked-down posture.
func NewClientWithOptions(timeout time.Duration, o Options) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		DialContext: ScreenedDialContext(o),
	}}
}

// NewClient returns an http.Client whose dialer blocks non-public destinations.
func NewClient(timeout time.Duration) *http.Client {
	return NewClientWithOptions(timeout, Options{})
}
