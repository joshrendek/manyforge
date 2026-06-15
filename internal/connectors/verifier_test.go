package connectors

import (
	"context"
	"errors"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// fakeVerifyConnector is a fakeConnector that also implements the optional VerifyAuth probe.
type fakeVerifyConnector struct {
	fakeConnector
	verifyErr error
	called    bool
}

func (f *fakeVerifyConnector) VerifyAuth(context.Context) error { f.called = true; return f.verifyErr }

func regWith(connType string, f Factory) *Registry {
	r := NewRegistry(nil) // svc unused: BuildSystem only needs the factory
	r.Register(connType, f)
	return r
}

func TestRegistryVerifier_ProbesViaClient(t *testing.T) {
	fc := &fakeVerifyConnector{}
	reg := regWith("jira", func(ResolvedConnector) (TicketingConnector, error) { return fc, nil })
	if err := NewRegistryVerifier(reg).Verify(context.Background(), VerifyTarget{Type: "jira"}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !fc.called {
		t.Fatal("VerifyAuth was not invoked")
	}
}

func TestRegistryVerifier_PropagatesAuthFailure(t *testing.T) {
	sentinel := errors.New("auth rejected")
	reg := regWith("jira", func(ResolvedConnector) (TicketingConnector, error) {
		return &fakeVerifyConnector{verifyErr: sentinel}, nil
	})
	if err := NewRegistryVerifier(reg).Verify(context.Background(), VerifyTarget{Type: "jira"}); !errors.Is(err, sentinel) {
		t.Fatalf("Verify = %v, want it to wrap the probe error", err)
	}
}

func TestRegistryVerifier_UnsupportedType(t *testing.T) {
	// A client without the VerifyAuth probe is a validation error, never a silent "ok".
	reg := regWith("jira", func(ResolvedConnector) (TicketingConnector, error) { return &fakeConnector{}, nil })
	if err := NewRegistryVerifier(reg).Verify(context.Background(), VerifyTarget{Type: "jira"}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("Verify = %v, want ErrValidation", err)
	}
}

func TestRegistryVerifier_UnknownType(t *testing.T) {
	if err := NewRegistryVerifier(NewRegistry(nil)).Verify(context.Background(), VerifyTarget{Type: "nope"}); err == nil {
		t.Fatal("Verify on unregistered type = nil, want error")
	}
}
