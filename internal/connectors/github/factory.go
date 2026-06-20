package github

import (
	"fmt"
	"strings"
	"time"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

// NewFactory returns a factory function that builds a GitHub RepoConnector from
// a ResolvedRepoConnector. The HTTP client is SSRF-safe (netsafe), with loopback
// and private addresses gated by rc.AllowPrivateBaseURL (same pattern as Jira factory).
func NewFactory(timeout time.Duration) func(connectors.ResolvedRepoConnector) (connectors.RepoConnector, error) {
	return func(rc connectors.ResolvedRepoConnector) (connectors.RepoConnector, error) {
		if rc.Credential.APIToken == "" {
			return nil, fmt.Errorf("github: factory: api_token required")
		}
		if !strings.Contains(rc.Repo, "/") {
			return nil, fmt.Errorf("github: factory: repo must be owner/name")
		}

		base := rc.BaseURL
		if base == "" {
			base = "https://api.github.com"
		}

		hc := netsafe.NewClientWithOptions(timeout, netsafe.Options{
			AllowLoopback: rc.AllowPrivateBaseURL,
			AllowPrivate:  rc.AllowPrivateBaseURL,
		})

		return &client{
			http:    hc,
			apiBase: strings.TrimSuffix(base, "/"),
			repo:    rc.Repo,
			token:   rc.Credential.APIToken,
		}, nil
	}
}
