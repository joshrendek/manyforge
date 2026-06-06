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

// NewClientWithOptions builds a guarded client configured by o. See Options for the
// available trust flags; the zero-value Options is the fully locked-down posture.
func NewClientWithOptions(timeout time.Duration, o Options) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if blockedWith(ip.IP, o.AllowLoopback, o.AllowPrivate) {
					return nil, fmt.Errorf("blocked address %s for host %s", ip.IP, host)
				}
			}
			// Dial the first resolved IP directly so we connect to the
			// address we vetted (avoids a TOCTOU re-resolution).
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
	}}
}

// NewClient returns an http.Client whose dialer blocks non-public destinations.
func NewClient(timeout time.Duration) *http.Client {
	return NewClientWithOptions(timeout, Options{})
}
