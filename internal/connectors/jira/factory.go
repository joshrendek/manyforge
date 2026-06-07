package jira

import (
	"fmt"
	"time"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// NewFactory returns a connectors.Factory that builds a Jira Cloud REST client
// for the given resolved connector. The HTTP client is SSRF-safe (netsafe)
// with allowLoopback/allowPrivate gated by rc.AllowPrivateBaseURL.
func NewFactory(timeout time.Duration) connectors.Factory {
	return func(rc connectors.ResolvedConnector) (connectors.TicketingConnector, error) {
		if rc.BaseURL == "" {
			return nil, fmt.Errorf("jira: factory: base_url is required")
		}
		if rc.Credential.Email == "" || rc.Credential.APIToken == "" {
			return nil, fmt.Errorf("jira: factory: email and api_token are required")
		}

		httpClient := netsafe.NewClientWithOptions(timeout, netsafe.Options{
			AllowLoopback: rc.AllowPrivateBaseURL,
			AllowPrivate:  rc.AllowPrivateBaseURL,
		})

		return &client{
			httpClient:    httpClient,
			baseURL:       rc.BaseURL,
			email:         rc.Credential.Email,
			apiToken:      rc.Credential.APIToken,
			webhookSecret: rc.Credential.WebhookSecret,
		}, nil
	}
}
