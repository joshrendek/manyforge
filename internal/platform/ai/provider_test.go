package ai

import (
	"context"
	"errors"
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
	if !errors.Is(ErrBadRequest, ErrBadRequest) {
		t.Fatal("sentinel identity broken")
	}
}
