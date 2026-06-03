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

	"github.com/manyforge/manyforge/internal/platform/ai"
)

// Behavioral pin: an openai-compat provider built by the factory with a private
// base_url fails (dial refused by netsafe) — it does NOT reach the host.
func TestAIProviderFactory_RefusesPrivateBaseURL(t *testing.T) {
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
			_, err = p.Complete(context.Background(), ai.Request{
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
	src := mustRead(t, path)
	if strings.Contains(src, "http.DefaultClient") {
		t.Fatalf("factory.go references http.DefaultClient — prod path must use netsafe only")
	}
}
