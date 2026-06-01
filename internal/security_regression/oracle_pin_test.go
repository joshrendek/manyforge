// No build tag: these source-level pins run in both `make test` and
// `make sec-test` with no infrastructure, so a refactor that drops an oracle
// defense fails CI even if a behavioral test is also weakened (SC-010 / FR-026).

package security_regression

import (
	"strings"
	"testing"
)

// TestOracleDefensesPinned asserts each enumeration/timing defense still exists.
func TestOracleDefensesPinned(t *testing.T) {
	cases := []struct {
		name, path, fragment string
	}{
		// Login email-miss runs a fixed-cost dummy hash so it can't be timed apart
		// from the wrong-password branch.
		{"login fixed-cost", "../account/service.go", "auth.DummyVerify(password)"},
		// A deactivated account fails with the same generic credential error (no oracle).
		{"deactivated uniform", "../account/service.go", `acc.Status != "active"`},
		// Invitation accept collapses invalid/expired/reused tokens to a single 410.
		{"invite-accept gone", "../invitations/handler.go", "http.StatusGone"},
		// Duplicate signup returns the same 202 as a fresh signup (no existence oracle).
		{"signup uniform 202", "../account/handler.go", "no existence oracle"},
		// SMTP RCPT rejects every unrouted recipient with ONE shared 550 error value,
		// so unknown-address / not-handled / not-yours are indistinguishable
		// (MF-002-INGEST-SCOPE, FR-003/SC-006). Pin the single shared error and that
		// the RCPT handler returns it (not a per-reason reply).
		{"smtp rcpt single reject", "../inbox/smtp.go", "var errGenericReject = &smtp.SMTPError{"},
		{"smtp rcpt no-oracle return", "../inbox/smtp.go", "return errGenericReject"},
		{"smtp canroute swallows detail", "../inbox/smtp.go", "func (s *Service) CanRoute("},
	}
	for _, c := range cases {
		if !strings.Contains(mustRead(t, c.path), c.fragment) {
			t.Errorf("%s: oracle-defense fragment %q missing from %s — was the defense removed?", c.name, c.fragment, c.path)
		}
	}
}
