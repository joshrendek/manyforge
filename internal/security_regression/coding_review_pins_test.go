package security_regression

import (
	"reflect"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

// MF007-PIN-1: the slice-1 repo connector must expose no code-write capability.
func TestRepoConnectorHasNoWriteCapability(t *testing.T) {
	typ := reflect.TypeOf((*connectors.RepoConnector)(nil)).Elem()
	banned := []string{"Push", "Commit", "CreatePR", "CreatePullRequest", "OpenPR", "Merge", "Write"}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		for _, b := range banned {
			if strings.Contains(name, b) {
				t.Fatalf("RepoConnector exposes write-capable method %q (banned substring %q) — slice 1 is read-only", name, b)
			}
		}
	}
}
