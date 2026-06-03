// Finding: US1b / Spec 003 §3.5 — a user-supplied openai-compat base_url MUST
// route through the SSRF-guarded netsafe client and cannot reach RFC1918 /
// loopback / metadata IPs. See manyforge-ma9.
package security_regression

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/ai"
)

// Behavioral pin: an openai-compat provider built by the factory with a private
// base_url fails (dial refused by netsafe) — it does NOT reach the host.
func TestAIProviderFactory_RefusesPrivateBaseURL(t *testing.T) {
	// Note: 127.0.0.1 is blocked by netsafe at the IP check (Blocked → IsLoopback),
	// refused BEFORE any dial — not a mere "connection refused". The RFC1918/metadata
	// IPs (10.0.0.1, 192.168.1.1, 169.254.169.254) have no local listener, so without
	// netsafe they would hang to timeout, not fail instantly — the instant pass here
	// is itself evidence the dial was refused, and TestAIFactory_UsesNetsafeSource
	// independently pins the wiring.
	privates := []string{
		"http://10.0.0.1/v1",
		"http://127.0.0.1:9999/v1",
		"http://169.254.169.254/v1",       // cloud metadata
		"http://192.168.1.1/v1",
	}
	for _, base := range privates {
		t.Run(base, func(t *testing.T) {
			p, err := ai.New(ai.Credential{Provider: "openai", APIKey: "k", BaseURL: base, Model: "gpt-4o"})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Self-protecting deadline: a future netsafe regression that lets the
			// dial proceed would otherwise hang to the client's ~60s timeout. A
			// short ctx makes such a regression fail FAST instead.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = p.Complete(ctx, ai.Request{
				MaxTokens: 16, Messages: []ai.Message{{Role: ai.RoleUser, Text: "x"}},
			})
			if !errors.Is(err, ai.ErrProviderUnavailable) {
				t.Fatalf("private base_url %q -> err %v, want Is(ErrProviderUnavailable) (dial refused)", base, err)
			}
		})
	}
}

// Source-level pin: the factory constructs its HTTP client via netsafe. A
// refactor that drops netsafe.NewClient from factory.go fails here loudly, even
// if behavior were masked.
func TestAIFactory_UsesNetsafeSource(t *testing.T) {
	const path = "../platform/ai/factory.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "netsafe" && sel.Sel.Name == "NewClient" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatalf("factory.go no longer calls netsafe.NewClient — SSRF guard dropped")
	}
	// Belt-and-suspenders: the prod factory must NOT fall back to a bare client.
	// mustRead is the package's existing untagged helper (escalation_pin_test.go).
	//
	// Scope: factory.go is the SOLE production entry point for building providers and
	// must never use a bare client. The transport constructors keep a nil→DefaultClient
	// fallback as an intentional test/record-injection seam (e.g. the AI_RECORD helpers
	// hit public provider APIs directly), so we deliberately pin factory.go — not the
	// constructors — as the SSRF-authoritative site. (US3 routes all prod construction
	// through ai.New.)
	src := mustRead(t, path)
	if strings.Contains(src, "http.DefaultClient") {
		t.Fatalf("factory.go references http.DefaultClient — prod path must use netsafe only")
	}
}
