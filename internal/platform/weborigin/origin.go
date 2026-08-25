// Package weborigin validates and canonicalizes browser origins used as analytics data-integrity
// controls. An Origin header is caller-controlled metadata, not authentication.
package weborigin

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// MaxAllowed bounds both persisted configuration and request/response payloads.
const MaxAllowed = 10

// Normalize returns an exact serialized origin: lower-case scheme/host, no default port, and no
// path, query, fragment, userinfo, or wildcard. Public origins must use HTTPS; HTTP is accepted
// only for localhost or a loopback literal so local development remains possible.
func Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("web origin must be an absolute URL")
	}
	scheme := strings.ToLower(u.Scheme)
	hostname, err := canonicalHost(u.Hostname())
	if err != nil {
		return "", err
	}
	if scheme != "https" && (scheme != "http" || !isLoopback(hostname)) {
		return "", fmt.Errorf("web origin must use https (http is allowed only for loopback)")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("web origin must not contain userinfo, a path, query, or fragment")
	}
	if strings.Contains(hostname, "*") || strings.ContainsAny(hostname, " \t\r\n,") {
		return "", fmt.Errorf("web origin host must be exact")
	}
	port := u.Port()
	if port == "" && strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("web origin port is empty")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("web origin port is invalid")
		}
		port = strconv.Itoa(n)
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}
	return scheme + "://" + host, nil
}

func canonicalHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(raw, "."))
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	if len(host) == 0 || len(host) > 253 {
		return "", fmt.Errorf("web origin host is invalid")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("web origin host is invalid")
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", fmt.Errorf("web origin host is invalid")
			}
		}
	}
	return host, nil
}

// FromHeader accepts exactly one Origin field value. Multiple fields, comma-joined values,
// opaque "null", and malformed values are rejected instead of choosing one attacker-controlled
// candidate. The caller can still return a uniform public response.
func FromHeader(values []string) (string, error) {
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", fmt.Errorf("exactly one Origin header is required")
	}
	return Normalize(values[0])
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
