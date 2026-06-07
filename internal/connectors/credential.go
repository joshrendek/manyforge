package connectors

import (
	"fmt"
	"net"
	"net/url"

	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

func validate(in CreateConnectorInput) error {
	if !knownConnectorTypes[in.Type] {
		return fmt.Errorf("connectors: unknown type %q: %w", in.Type, errs.ErrValidation)
	}
	if in.DisplayName == "" {
		return fmt.Errorf("connectors: display_name required: %w", errs.ErrValidation)
	}
	if in.BaseURL == "" {
		return fmt.Errorf("connectors: base_url required: %w", errs.ErrValidation)
	}
	if err := validateBaseURL(in.BaseURL, in.AllowPrivateBaseURL); err != nil {
		return err
	}
	if in.Email == "" || in.APIToken == "" {
		return fmt.Errorf("connectors: email and api_token required: %w", errs.ErrValidation)
	}
	return nil
}

// validateBaseURL pins URL shape and, for a LITERAL IP host, applies the exact
// netsafe dialer policy (metadata/link-local always blocked; private/loopback only
// with the trust flag). Hostnames are NOT resolved here — dial-time netsafe stays
// authoritative against DNS rebinding. Mirrors agents.validateBaseURL.
func validateBaseURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("connectors: base_url must be a valid http(s) URL: %w", errs.ErrValidation)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if netsafe.IsBlocked(ip, netsafe.Options{AllowLoopback: allowPrivate, AllowPrivate: allowPrivate}) {
			return fmt.Errorf("connectors: base_url %q is a blocked address: %w", raw, errs.ErrValidation)
		}
	}
	return nil
}
