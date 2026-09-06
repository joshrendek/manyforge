package security_regression

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

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

// MF-MAIL-DELIVERY-003 requires the development log sink to emit only
// non-capability metadata and to report that no provider accepted the message.
func TestMFMailDelivery003UnsetSMTPIsNonAcceptingAndCapabilitySafe(t *testing.T) {
	mainSource := auditSource(t, "../../cmd/manyforge/main.go")
	if !strings.Contains(mainSource, "cfg.Environment == \"development\"") {
		t.Fatal("SMTP-unset fallback is not restricted to explicit development mode")
	}

	var logs bytes.Buffer
	sender := notify.LogSender{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	err := sender.Send(context.Background(), notify.Mail{
		From: "news@example.test", To: "recipient@example.test", Subject: "Campaign",
		BodyText: "unsubscribe: https://manyforge.test/m/u/audit-capability-token",
	})
	if err == nil {
		t.Fatal("LogSender reported provider acceptance")
	}
	for _, secret := range []string{"news@example.test", "recipient@example.test", "Campaign", "audit-capability-token"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("LogSender output leaked %q: %s", secret, logs.String())
		}
	}
}

// MF-MAIL-DELIVERY-004 requires one Tick to process each claimed campaign at
// most once under explicit global and per-campaign row budgets.
func TestMFMailDelivery004TickHasGlobalAndPerCampaignFanoutBounds(t *testing.T) {
	source := auditSource(t, "../../internal/mailing/sendworker.go")
	tick := auditSection(t, source, "func (w *SendWorker) Tick", "func (w *SendWorker) claimCampaigns")
	for _, marker := range []string{"fanoutGlobalBudget", "fanoutPerCampaignBudget"} {
		if !strings.Contains(source, marker) {
			t.Fatalf("worker is missing explicit fan-out bound %q", marker)
		}
	}
	if strings.Contains(tick, "for {") {
		t.Fatal("Tick still contains an unbounded campaign fan-out loop")
	}
}

// MF-MAIL-ERRACK-001 requires authenticated provider persistence failures to
// return a generic retryable response rather than a success acknowledgement.
func TestMFMailErrack001ProviderDurabilityErrorsReturnRetryableFailure(t *testing.T) {
	resend := auditSource(t, "../../internal/mailing/webhook_resend.go")
	ses := auditSource(t, "../../internal/mailing/webhook_ses.go")
	bounce := auditSource(t, "../../internal/inbox/bounce.go")
	webhookCore := auditSource(t, "../../internal/mailing/webhook.go")
	for name, source := range map[string]string{"resend": resend, "ses": ses} {
		if !strings.Contains(source, "failed") || !strings.Contains(source, "retryableFailure") {
			t.Fatalf("%s path does not preserve a retryable authenticated durability failure", name)
		}
	}
	if !strings.Contains(webhookCore, "w.WriteHeader(http.StatusServiceUnavailable)") {
		t.Fatal("provider retryable failure helper is not a generic 503")
	}
	if !strings.Contains(resend, "mailing Resend webhook apply failed") ||
		!strings.Contains(resend, "h.retryableFailure(w)") {
		t.Fatal("Resend apply errors are not returned as a generic retryable failure")
	}
	if !strings.Contains(ses, "mailing SES webhook apply failed") ||
		!strings.Contains(ses, "h.retryableFailure(w)") {
		t.Fatal("SES apply errors are not returned as a generic retryable failure")
	}
	if !strings.Contains(bounce, "inbox: bounce suppression failed") ||
		!strings.Contains(bounce, "w.WriteHeader(http.StatusServiceUnavailable)") {
		t.Fatal("relay bounce suppression errors are not returned as a generic retryable failure")
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

// MF-MAIL-FEEDBACK-001 requires complete feedback configuration, separate
// durable readiness, and readiness fences at admission, claim, and renewal.
func TestMFMailFeedback001RequiresDurableFeedbackReadiness(t *testing.T) {
	profileSource := auditSource(t, "../../internal/mailing/profile.go")
	putProfile := auditSection(t, profileSource, "func (s *Service) PutSendingProfile(", "func (s *Service) DeleteSendingProfile(")
	verifySource := auditSource(t, "../../internal/mailing/profile_delivery.go")
	verifyProfile := auditSection(t, verifySource, "func (s *Service) VerifySendingProfile(", "func (s *Service) TestSendingProfile(")
	typesSource := auditSource(t, "../../internal/mailing/types.go")
	resendProvider := auditSource(t, "../../internal/mailing/provider/resend.go")
	sesProvider := auditSource(t, "../../internal/mailing/provider/ses.go")
	scheduleSource := auditSource(t, "../../db/query/mailing.sql")
	schedule := auditSection(t, scheduleSource, "-- name: ScheduleCampaign", "-- name: ListCampaignDeliveries")
	migration := auditSource(t, "../../migrations/0134_mailing_provider_feedback.up.sql")
	ses := auditSource(t, "../../internal/mailing/webhook_ses.go")
	resendWebhook := auditSource(t, "../../internal/mailing/webhook_resend.go")

	for _, marker := range []string{
		"validateSESFeedbackConfiguration", "SESConfigurationSet", "SNSTopicARN",
	} {
		if !strings.Contains(putProfile, marker) {
			t.Fatalf("provider setup does not require feedback configuration marker %q", marker)
		}
	}
	publicResend := auditSection(t, typesSource, "type ResendCredentials struct", "type resendStoredCredentials struct")
	if strings.Contains(publicResend, "WebhookSecret") {
		t.Fatal("tenant-facing Resend credentials accept a signing secret")
	}
	storedResend := auditSection(t, typesSource, "type resendStoredCredentials struct", "type SESCredentials struct")
	if !strings.Contains(storedResend, "Version") ||
		!strings.Contains(resendWebhook, "creds.Version != 2") ||
		!strings.Contains(resendWebhook, `strings.TrimSpace(creds.WebhookID) == ""`) {
		t.Fatal("Resend ingestion does not require the versioned provider-provisioned credential bundle")
	}
	for _, marker := range []string{
		"ResendWebhookProvisioner", "EnsureWebhook", "persistResendWebhookVerification",
	} {
		if !strings.Contains(verifyProfile, marker) {
			t.Fatalf("Resend verification is missing control-plane attestation marker %q", marker)
		}
	}
	for _, marker := range []string{`"endpoint"`, `"email.bounced"`, `"email.complained"`, `"signing_secret"`} {
		if !strings.Contains(resendProvider, marker) {
			t.Fatalf("Resend control-plane provisioning missing marker %q", marker)
		}
	}
	for _, marker := range []string{`"/webhooks?limit=100"`, "listed.HasMore", "reconcileWebhooks", "DeleteWebhook"} {
		if !strings.Contains(resendProvider, marker) {
			t.Fatalf("Resend control-plane reconciliation missing marker %q", marker)
		}
	}
	if !strings.Contains(resendProvider, "requireMatch") ||
		!strings.Contains(resendProvider, "replacement Resend key did not find") ||
		!strings.Contains(resendProvider, "cleanup was not confirmed") {
		t.Fatal("unbound replacement Resend key can prove cleanup without a positive match and post-delete confirmation")
	}
	for _, marker := range []string{"ClaimMailingResendProvisioning", "resend_provisioning_token", "resend_provisioning_expires_at", "resend_cleanup_required"} {
		if !strings.Contains(scheduleSource, marker) {
			t.Fatalf("Resend provisioning recovery intent missing marker %q", marker)
		}
	}
	if !strings.Contains(migration, "WHERE mode = 'resend'") ||
		!strings.Contains(migration, "feedback_status = 'pending'") ||
		!strings.Contains(migration, "status = 'unverified'") ||
		!strings.Contains(migration, "resend_cleanup_required") {
		t.Fatal("migration does not fail closed for legacy Resend credential bundles")
	}
	if !strings.Contains(putProfile, "cleanupResendWebhooks") ||
		!strings.Contains(resendProvider, "CleanupWebhooks") {
		t.Fatal("profile mutation does not require exact-endpoint Resend cleanup")
	}
	for _, marker := range []string{"GetConfigurationSetEventDestinations", "ExpectedTopicARN", "EventTypeBounce", "EventTypeComplaint"} {
		if !strings.Contains(sesProvider, marker) {
			t.Fatalf("SES route attestation missing marker %q", marker)
		}
	}
	if !strings.Contains(schedule, "p.status = 'verified'") ||
		!strings.Contains(schedule, "p.feedback_status = 'ready'") {
		t.Fatal("campaign admission does not require outbound verification and feedback readiness")
	}
	for _, marker := range []string{
		"mailing_transition_ses_feedback", "p.feedback_status = 'ready'",
		"mailing_claim_deliveries", "mailing_renew_delivery",
	} {
		if !strings.Contains(migration, marker) {
			t.Fatalf("provider feedback migration missing readiness marker %q", marker)
		}
	}
	if !strings.Contains(ses, `h.SNS.Confirm(r.Context(), envelope.SubscribeURL, *wc.snsTopicARN)`) ||
		!strings.Contains(ses, `h.transitionSESFeedback(r.Context(), wc, "ready", "")`) {
		t.Fatal("SES confirmation is not bound to its topic and durably transitioned to ready")
	}
}
