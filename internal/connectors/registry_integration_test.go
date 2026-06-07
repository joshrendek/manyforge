//go:build integration

package connectors

import (
	"errors"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestRegistryResolveBindsCredential(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		// The factory receives the resolved, unsealed credential + base_url.
		if rc.Credential.APIToken != "tok-abc-123" {
			t.Fatalf("factory did not receive unsealed credential: %+v", rc.Credential)
		}
		return &fakeConnector{issue: ExternalIssue{ExternalID: "JIRA-1", URL: rc.BaseURL}}, nil
	})
	reg.Register("zendesk", func(rc ResolvedConnector) (TicketingConnector, error) {
		t.Fatalf("zendesk factory must not be called for a jira connector")
		return nil, nil
	})

	c, err := reg.Resolve(ctx, seed.principalID, seed.businessID, connID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	iss, err := c.FetchIssue(ctx, "JIRA-1")
	if err != nil || iss.URL != "https://acme.atlassian.net" {
		t.Fatalf("expected fake bound to base_url, got %+v err %v", iss, err)
	}
}

func TestRegistryUnregisteredTypeErrors(t *testing.T) {
	ctx, tdb, seed := startConn(t)
	svc := newConnService(t, tdb, nil)
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	reg := NewRegistry(svc) // no factories registered
	_, err = reg.Resolve(ctx, seed.principalID, seed.businessID, connID)
	if err == nil {
		t.Fatalf("expected error for unregistered connector type")
	}
	if !strings.Contains(err.Error(), "jira") {
		t.Fatalf("error should name the connector type, got: %v", err)
	}
}

func TestRegistryResolveCrossTenantNotFound(t *testing.T) {
	ctx, tdb, a := startConn(t)
	b := seedConnectorTenant(ctx, t, tdb)
	svc := newConnService(t, tdb, nil)
	connID, err := svc.Create(ctx, a.principalID, a.businessID, jiraInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		return &fakeConnector{}, nil
	})
	_, err = reg.Resolve(ctx, b.principalID, b.businessID, connID)
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("want ErrNotFound for cross-tenant resolve, got %v", err)
	}
}
