// Finding: SECREVIEW-M6 — Authentication-Results shown to agents could be
// supplied by the sender.
//
// The parser trusted the FIRST Authentication-Results header with no check of its
// authserv-id, so a sender writing "Authentication-Results: attacker.invalid;
// spf=pass; dkim=pass; dmarc=pass" could make a forged From look fully verified
// in the agent UI. Verdicts are now taken only from the first header whose
// authserv-id is in MANYFORGE_INBOUND_TRUSTED_AUTHSERV_IDS (default
// amazonses.com, the id Resend's receiving pipeline stamps); with none trusted
// every verdict is unknown. The trust applies only to the Resend path: the SMTP
// receiver and JSON /inbound webhook have no SES hop above the sender's headers,
// so they trust none.
//
// Behavioural coverage: internal/inbox/source_auth_test.go, the integration test
// internal/inbox/auth_results_integration_test.go, and
// internal/platform/config/inbound_auth_config_test.go. These pins fail loudly if
// a refactor reintroduces the untrusted first-header read or drops the plumbing.
package security_regression

import (
	"strings"
	"testing"
)

func TestPin_SECREVIEW_M6_AuthResultsFromTrustedAuthservOnly(t *testing.T) {
	src := mustRead(t, "../inbox/source.go")
	if strings.Contains(src, `parseAuthResults(env.GetHeader("Authentication-Results"))`) {
		t.Error("Parse must not trust the first Authentication-Results header unconditionally")
	}
	if !strings.Contains(src, `env.GetHeaderValues("Authentication-Results")`) {
		t.Error("Parse must keep every Authentication-Results header so the trusted one can be selected")
	}
	fn := mf017Slice(t, src, "func trustedAuthResults(", "\n}\n")
	if !strings.Contains(fn, "authservID(h)") || !strings.Contains(fn, "strings.EqualFold(") {
		t.Error("trustedAuthResults must match each header's authserv-id against the trusted set (case-insensitive)")
	}

	svc := mustRead(t, "../inbox/service.go")
	ingest := mf017Slice(t, svc, "func (s *Service) Ingest(", "func (s *Service) sniffAttachments(")
	if !strings.Contains(ingest, "trustedAuthResults(parsed.AuthenticationResults, s.trustedAuthservFor(msg.Provider))") {
		t.Error("Ingest must store only the trusted-authserv verdicts for the message's transport")
	}
	scope := mf017Slice(t, svc, "func (s *Service) trustedAuthservFor(", "\n}\n")
	if !strings.Contains(scope, "if provider == resendSource {") || !strings.Contains(scope, "return nil") {
		t.Error("only the Resend path may trust the configured authserv-ids; SMTP and the JSON webhook trust none")
	}

	cfg := mustRead(t, "../platform/config/config.go")
	if !strings.Contains(cfg, `os.LookupEnv("MANYFORGE_INBOUND_TRUSTED_AUTHSERV_IDS")`) ||
		!strings.Contains(cfg, `defaultInboundTrustedAuthservIDs = "amazonses.com"`) {
		t.Error("MANYFORGE_INBOUND_TRUSTED_AUTHSERV_IDS must be loaded with the amazonses.com default")
	}
	main := mustRead(t, "../../cmd/manyforge/main.go")
	if !strings.Contains(main, "TrustedAuthservIDs:  cfg.InboundTrustedAuthservIDs") {
		t.Error("the configured trusted authserv-ids must be plumbed into the inbox service")
	}
}
