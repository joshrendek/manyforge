// No build tag: this source-level pin runs in `make test` and `make sec-test` with
// no infrastructure (T067 / SC-006 / FR-015). The behavioral byte-identical-404 proof
// (unknown vs cross-tenant vs unauthorized GET) lives in
// internal/ticketing/oracle_integration_test.go (Docker-gated). This pin locks the
// no-oracle 404-collapse at the source level two ways a behavioral test can't cheaply
// guarantee for ALL endpoints at once:
//   1. no support handler ever emits a 403 (http.StatusForbidden) — authorization and
//      existence must be indistinguishable; and
//   2. the central error renderer maps BOTH ErrNotFound and ErrForbidden to the same
//      404 NOT_FOUND body, so a service that returns ErrForbidden still cannot leak a
//      "this exists but isn't yours" oracle.

package security_regression

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoForbiddenInSupportHandlersPinned sweeps every non-test Go file in the
// ticketing and inbox packages and asserts none references http.StatusForbidden.
// A glob (not a fixed list) so a future handler file is covered automatically.
func TestNoForbiddenInSupportHandlersPinned(t *testing.T) {
	for _, dir := range []string{"../ticketing", "../inbox"} {
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		var scanned int
		for _, path := range matches {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			scanned++
			if strings.Contains(string(b), "StatusForbidden") {
				t.Errorf("%s references StatusForbidden — support endpoints must collapse unauthorized to 404, never 403 (FR-015/SC-006)", path)
			}
		}
		if scanned == 0 {
			t.Errorf("no non-test Go files scanned in %s — glob/path wrong, pin is not actually guarding anything", dir)
		}
	}
}

// TestErrorRendererCollapsesForbiddenTo404Pinned pins that the central WriteError
// maps ErrNotFound AND ErrForbidden to the identical 404 NOT_FOUND body. This is the
// rendering-layer half of the no-oracle guarantee: even if a service returns
// ErrForbidden, the wire response is indistinguishable from not-found.
func TestErrorRendererCollapsesForbiddenTo404Pinned(t *testing.T) {
	errsrc := mustRead(t, "../platform/httpx/errors.go")
	for _, fragment := range []string{
		// Both sentinels share one case arm...
		"case errors.Is(err, errs.ErrNotFound), errors.Is(err, errs.ErrForbidden):",
		// ...that renders a 404 with the generic NOT_FOUND body.
		`WriteJSON(w, http.StatusNotFound, ErrorBody{Code: "NOT_FOUND", Message: "not found"})`,
	} {
		if !strings.Contains(errsrc, fragment) {
			t.Errorf("oracle-collapse pin: httpx/errors.go missing %q — ErrForbidden must render an identical 404, never a distinguishable 403 (FR-015)", fragment)
		}
	}
}
