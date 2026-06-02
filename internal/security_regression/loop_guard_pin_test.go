// No build tag: this source-level pin runs in `make test` and `make sec-test` with
// no infrastructure (manyforge-axq / T070). It guards the FR-018 / SC-011 mail-loop
// suppression wired into the ingest_inbound_message DEFINER (migration 0024) and the
// Go ingest path. Without the per-requester auto-reply cap, two auto-responders can
// amplify unboundedly; a refactor that drops the cap, the suppression audit, or the
// Go honouring of it must fail CI loudly even if the integration loop test is also
// weakened.

package security_regression

import (
	"strings"
	"testing"
)

// TestLoopGuardPinned asserts the loop-guard cap + audit stay in the ingestion
// DEFINER, and that the service still passes the bound and honours suppression.
func TestLoopGuardPinned(t *testing.T) {
	mig := mustRead(t, "../../migrations/0024_loop_guard.up.sql")
	svc := mustRead(t, "../inbox/service.go")
	cases := []struct {
		name, src, fragment string
	}{
		// The DEFINER gates ONLY auto-generated mail, by a positive per-requester cap.
		{"cap parameter", mig, "p_loop_max_auto_replies integer"},
		{"only auto mail capped", mig, "COALESCE(p_is_auto_reply, false) AND p_loop_max_auto_replies > 0"},
		{"window count of auto-replies", mig, "AND tm.is_auto_reply"},
		{"cap comparison", mig, "v_recent_auto >= p_loop_max_auto_replies"},
		// Suppression writes a principal-less audit event and inserts nothing.
		{"suppression audited", mig, "'ticket.loop_suppressed'"},
		{"suppress inserts nothing", mig, "RETURN QUERY SELECT NULL::uuid, NULL::uuid, false, false, true;"},
		// The Go ingest path passes the bound and short-circuits on suppression.
		{"service passes the bound", svc, "s.loopGuardMax()"},
		{"service honours suppression", svc, "if out.Suppressed {"},
	}
	for _, c := range cases {
		if !strings.Contains(c.src, c.fragment) {
			t.Errorf("%s: loop-guard pin %q missing — was FR-018/SC-011 suppression dropped or weakened?", c.name, c.fragment)
		}
	}
}
