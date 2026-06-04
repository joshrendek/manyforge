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

// blockedWith reports whether ip must be refused. allowLoopback permits 127/8 + ::1
// ONLY (for dev MCP servers); all other private/link-local/metadata stay blocked.
func blockedWith(ip net.IP, allowLoopback bool) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() {
		return !allowLoopback
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	for _, m := range metadataIPs {
		if ip.Equal(m) {
			return true
		}
	}
	return false
}

// Blocked reports whether ip is a destination outbound requests must refuse
// (loopback blocked — the default, locked-secure posture).
func Blocked(ip net.IP) bool { return blockedWith(ip, false) }

// Options configures a guarded client.
type Options struct{ AllowLoopback bool }

// NewClientWithOptions builds a guarded client; AllowLoopback permits loopback only.
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
				if blockedWith(ip.IP, o.AllowLoopback) {
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
