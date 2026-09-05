package security_regression

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

func auditSource(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func auditSection(t *testing.T, source, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(source, startMarker)
	if start < 0 {
		t.Fatalf("missing start marker %q", startMarker)
	}
	rest := source[start:]
	end := strings.Index(rest, endMarker)
	if end < 0 {
		t.Fatalf("missing end marker %q", endMarker)
	}
	return rest[:end]
}

// MF-AUTO-001 characterizes the activation/graph TOCTOU. Activate locks the
// automation but not the validated version row, while PutGraph can update that
// draft concurrently. Replace this pin with a behavioral regression after the
// version row or validated graph identity is fenced atomically.
func TestMFAuto001ActivationDoesNotFenceValidatedGraph(t *testing.T) {
	source := auditSource(t, "../../internal/automations/service.go")
	activate := auditSection(t, source, "func (s *Service) Activate", "func (s *Service) Pause")
	putGraph := auditSection(t, source, "func (s *Service) PutGraph", "func (s *Service) ValidateVersion")

	if !strings.Contains(activate, "lockAutomation(") ||
		!strings.Contains(activate, "q.GetAutomationVersion(") ||
		!strings.Contains(putGraph, "q.UpdateAutomationVersionGraph(") {
		t.Fatal("expected vulnerable activation and graph-update paths were not found")
	}
	if strings.Contains(activate, "FOR UPDATE") || strings.Contains(activate, "lockAutomationVersion") {
		t.Fatal("activation now appears to fence the version row; replace this characterization with the fixed invariant")
	}
}

// MF-AUTO-002 characterizes unbounded version retention and response loading.
// The list query has no LIMIT and the service decodes every full graph.
func TestMFAuto002VersionHistoryResponseHasNoBound(t *testing.T) {
	service := auditSource(t, "../../internal/automations/service.go")
	versions := auditSection(t, service, "func (s *Service) Versions(", "func (s *Service) Version(")
	queries := auditSource(t, "../../db/query/automations.sql")
	query := auditSection(t, queries, "-- name: ListAutomationVersions", "-- name: UpdateAutomationVersionGraph")

	if !strings.Contains(versions, "ListAutomationVersions") ||
		!strings.Contains(versions, "for _, row := range rows") {
		t.Fatal("expected full version-history decoding path was not found")
	}
	if strings.Contains(strings.ToUpper(query), "LIMIT") {
		t.Fatal("version history query is now bounded; replace this characterization with the fixed limit contract")
	}
}

// MF-MAIL-DELIVERY-003 characterizes the production-reachable SMTP fallback:
// main wires LogSender when the host is absent, and LogSender returns success
// after writing recipient and body capability data to logs.
func TestMFMailDelivery003UnsetSMTPLogsCapabilityAndReportsSuccess(t *testing.T) {
	mainSource := auditSource(t, "../../cmd/manyforge/main.go")
	if !strings.Contains(mainSource, "sender = notify.LogSender") ||
		!strings.Contains(mainSource, "MANYFORGE_SMTP_HOST unset") {
		t.Fatal("expected SMTP-unset LogSender fallback was not found")
	}

	var logs bytes.Buffer
	sender := notify.LogSender{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	err := sender.Send(context.Background(), notify.Mail{
		From: "news@example.test", To: "recipient@example.test", Subject: "Campaign",
		BodyText: "unsubscribe: https://manyforge.test/m/u/audit-capability-token",
	})
	if err != nil {
		t.Fatalf("LogSender returned an error: %v", err)
	}
	for _, secret := range []string{"recipient@example.test", "audit-capability-token"} {
		if !strings.Contains(logs.String(), secret) {
			t.Fatalf("LogSender output did not contain %q: %s", secret, logs.String())
		}
	}
}

// MF-MAIL-DELIVERY-004 characterizes the unbounded outer fan-out loop. One Tick
// drains each claimed campaign completely instead of honoring one batch budget.
func TestMFMailDelivery004TickLoopsUntilCampaignFanoutCompletes(t *testing.T) {
	source := auditSource(t, "../../internal/mailing/sendworker.go")
	tick := auditSection(t, source, "func (w *SendWorker) Tick", "func (w *SendWorker) claimCampaigns")
	for _, marker := range []string{
		"for _, campaignID := range campaignIDs",
		"for {",
		"done, err := w.fanout",
		"if done {",
		"break",
	} {
		if !strings.Contains(tick, marker) {
			t.Fatalf("Tick no longer contains unbounded fan-out marker %q", marker)
		}
	}
}

// MF-MAIL-ERRACK-001 characterizes authenticated mutation failures that are
// logged and then acknowledged as success. Invalid/unknown input may remain
// uniform; this pin covers the distinct internal-error path that suppresses retry.
func TestMFMailErrack001DurabilityErrorsAreAcknowledged(t *testing.T) {
	track := auditSource(t, "../../internal/mailing/track.go")
	resend := auditSource(t, "../../internal/mailing/webhook_resend.go")
	ses := auditSource(t, "../../internal/mailing/webhook_ses.go")
	bounce := auditSource(t, "../../internal/inbox/bounce.go")

	for name, source := range map[string]string{
		"unsubscribe":  track,
		"resend":       resend,
		"ses":          ses,
		"relay bounce": bounce,
	} {
		if !strings.Contains(source, "failed") {
			t.Fatalf("%s path no longer exposes the expected mutation-error branch", name)
		}
	}
	if !strings.Contains(track, "mailing unsubscribe failed") ||
		!strings.Contains(track, "w.WriteHeader(http.StatusOK)") {
		t.Fatal("unsubscribe no longer logs a durability error and then returns 200")
	}
	if !strings.Contains(resend, "mailing Resend webhook apply failed") ||
		!strings.Contains(resend, "h.authenticatedOK(w)") {
		t.Fatal("Resend no longer logs an apply error and then returns 200")
	}
	if !strings.Contains(ses, "mailing SES webhook apply failed") ||
		!strings.Contains(ses, "h.authenticatedOK(w)") {
		t.Fatal("SES no longer logs an apply error and then returns 200")
	}
	if !strings.Contains(bounce, "inbox: bounce suppression failed") ||
		!strings.Contains(bounce, "h.writeAccepted(w)") {
		t.Fatal("relay bounce no longer logs a suppression error and then returns 202")
	}
}

// MF-MAIL-LIFECYCLE-002 characterizes the missing archived-list predicate at
// principal-less activation and campaign fan-out sinks. Unsubscribe is excluded
// deliberately because it must remain available after archival.
func TestMFMailLifecycle002ArchivedListIsNotCheckedAtWorkerSinks(t *testing.T) {
	confirmSQL := auditSource(t, "../../migrations/0125_mailing_public_definers.up.sql")
	confirm := auditSection(t, confirmSQL, "CREATE FUNCTION mailing_confirm(", "CREATE FUNCTION mailing_unsubscribe(")
	campaignSQL := auditSource(t, "../../migrations/0126_mailing_campaigns.up.sql")
	claim := auditSection(t, campaignSQL, "CREATE FUNCTION mailing_claim_campaigns_for_fanout(", "CREATE FUNCTION mailing_fanout_batch(")
	fanout := auditSection(t, campaignSQL, "CREATE FUNCTION mailing_fanout_batch(", "CREATE FUNCTION mailing_claim_deliveries(")

	for name, body := range map[string]string{
		"confirmation":    confirm,
		"campaign claim":  claim,
		"campaign fanout": fanout,
	} {
		if strings.Contains(body, "mailing_list") {
			t.Fatalf("%s now consults mailing_list; replace this characterization with the fixed active-list invariant", name)
		}
	}
}

// MF-MAIL-LOG-001 characterizes capability-bearing public paths being written
// verbatim by the global request logger. The GET target is inert.
func TestMFMailLog001CapabilityTokenAppearsInRequestLog(t *testing.T) {
	const sentinel = "MF_CAPABILITY_SENTINEL"

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	router := httpx.NewRouter(nil)
	router.Get("/m/u/{token}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/m/u/"+sentinel, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(logs.String(), sentinel) {
		t.Fatalf("expected request log to contain capability token %q; log: %s", sentinel, logs.String())
	}
}

// AUTOMATION-SEND-AUTHZ-005 characterizes the write-only path from mutable
// automation content and explicit enrollment to provider-bound delivery work.
func TestAutomationSendAuthz005WritePermissionReachesProviderContent(t *testing.T) {
	permissions := auditSource(t, "../../migrations/0124_mailing_core.up.sql")
	automationHandler := auditSource(t, "../../internal/automations/handler.go")
	automationWrite := auditSection(t, automationHandler, "func (h *Handler) WriteRoutes(", "func (h *Handler) SendRoutes(")
	automationSend := auditSection(t, automationHandler, "func (h *Handler) SendRoutes(", "type nullableString struct")
	mailingHandler := auditSource(t, "../../internal/mailing/handler.go")
	mailingWrite := auditSection(t, mailingHandler, "func (h *Handler) WriteRoutes(", "// SendRoutes registers")
	claimSQL := auditSource(t, "../../migrations/0130_automation_engine_ports.up.sql")

	if !strings.Contains(permissions, "'mailing.write'") ||
		!strings.Contains(permissions, "'mailing.send'") {
		t.Fatal("expected distinct mailing.write and mailing.send permissions")
	}
	if !strings.Contains(automationWrite, "h.enroll") ||
		!strings.Contains(automationWrite, "h.createEvent") ||
		strings.Contains(automationSend, "h.enroll") ||
		strings.Contains(automationSend, "h.createEvent") {
		t.Fatal("expected enrollment and event injection to remain write-gated rather than send-gated")
	}
	if !strings.Contains(mailingWrite, "h.updateTemplate") {
		t.Fatal("expected mutable template content to remain write-gated")
	}
	if !strings.Contains(claimSQL, "COALESCE(c.subject, t.subject)") ||
		!strings.Contains(claimSQL, "COALESCE(c.body_markdown, t.body_markdown)") {
		t.Fatal("expected automation delivery claim to resolve current mutable template content")
	}
}

// MF-MAIL-FEEDBACK-001 characterizes outbound verification becoming send-ready
// without an authenticated, durable provider-feedback channel.
func TestMFMailFeedback001OutboundVerificationIgnoresFeedbackReadiness(t *testing.T) {
	profileSource := auditSource(t, "../../internal/mailing/profile.go")
	putProfile := auditSection(t, profileSource, "func (s *Service) PutSendingProfile(", "func (s *Service) DeleteSendingProfile(")
	verifySource := auditSource(t, "../../internal/mailing/profile_delivery.go")
	verifyProfile := auditSection(t, verifySource, "func (s *Service) VerifySendingProfile(", "func (s *Service) TestSendingProfile(")
	scheduleSource := auditSource(t, "../../db/query/mailing.sql")
	schedule := auditSection(t, scheduleSource, "-- name: ScheduleCampaign", "-- name: ListCampaignDeliveries")
	resend := auditSource(t, "../../internal/mailing/webhook_resend.go")
	ses := auditSource(t, "../../internal/mailing/webhook_ses.go")

	if !strings.Contains(putProfile, `if in.Resend.WebhookSecret != ""`) {
		t.Fatal("expected empty Resend webhook secret to remain accepted")
	}
	if strings.Contains(putProfile, "SNSTopicARN == nil") ||
		strings.Contains(putProfile, "SESConfigurationSet == nil") {
		t.Fatal("feedback configuration is now required; replace this characterization with the fixed readiness invariant")
	}
	if strings.Contains(verifyProfile, "WebhookSecret") ||
		strings.Contains(verifyProfile, "SNSTopic") ||
		strings.Contains(verifyProfile, "feedback") {
		t.Fatal("outbound verification now consults feedback readiness")
	}
	if !strings.Contains(schedule, "p.status = 'verified'") {
		t.Fatal("campaign admission no longer relies on outbound verification status")
	}
	if !strings.Contains(resend, `creds.WebhookSecret == ""`) ||
		!strings.Contains(ses, "wc.snsTopicARN == nil") {
		t.Fatal("expected missing feedback configuration to make inbound provider events unusable")
	}
}
