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

// NewClient returns an http.Client whose dialer blocks non-public destinations.
func NewClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
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
					if Blocked(ip.IP) {
						return nil, fmt.Errorf("blocked address %s for host %s", ip.IP, host)
					}
				}
				// Dial the first resolved IP directly so we connect to the
				// address we vetted (avoids a TOCTOU re-resolution).
				return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
			},
		},
	}
}

var metadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"),    // AWS/GCP/Azure IMDS
	net.ParseIP("fd00:ec2::254"),      // AWS IMDS IPv6
}

// Blocked reports whether ip is a destination outbound requests must refuse.
func Blocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	for _, m := range metadataIPs {
		if ip.Equal(m) {
			return true
		}
	}
	return false
}
