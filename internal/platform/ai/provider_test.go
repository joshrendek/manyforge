package ai

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// staticProvider is a throwaway to prove the interface is satisfiable.
type staticProvider struct{ resp Response }

func (s staticProvider) Complete(_ context.Context, _ Request) (Response, error) { return s.resp, nil }
func (s staticProvider) Name() string                                            { return "static" }

func TestProviderInterfaceSatisfied(t *testing.T) {
	var p Provider = staticProvider{resp: Response{Text: "ok"}}
	got, err := p.Complete(context.Background(), Request{})
	if err != nil || got.Text != "ok" {
		t.Fatalf("Complete = (%+v, %v)", got, err)
	}
	wrapped := fmt.Errorf("provider 400: bad model: %w", ErrBadRequest)
	if !errors.Is(wrapped, ErrBadRequest) {
		t.Fatal("wrapped ErrBadRequest must unwrap via errors.Is")
	}
	if errors.Is(ErrBadRequest, ErrProviderUnavailable) {
		t.Fatal("sentinels must not alias")
	}
}
