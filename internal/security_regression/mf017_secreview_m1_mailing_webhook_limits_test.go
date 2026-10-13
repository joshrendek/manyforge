// Finding: SECREVIEW-M1 — Resend traffic for every tenant shared the provider's
// source-IP rate-limit bucket. The mailing provider webhook sat behind the low
// per-IP end-user ingest limiter (2 rps / burst 20) BEFORE signature
// verification, so one flooded support address exhausted the bucket for Resend's
// shared egress IP and 429ed every other tenant's delivery/bounce/received events.
//
// Fix: the mailing provider webhooks are mounted outside the ingest group behind a
// generous pre-auth per-IP ceiling (MANYFORGE_MAILING_WEBHOOK_IP_RATE_*), and a
// per-sending-profile budget (MANYFORGE_MAILING_WEBHOOK_PROFILE_RATE_*) applies
// only AFTER the Svix/SNS signature verifies. The behavioural tests live in
// cmd/manyforge (route wiring), internal/mailing (per-profile budget, unit +
// integration); these pins keep the shape from regressing.
package security_regression

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

func TestPin_SecreviewM1MailingWebhookNotBehindIngestLimit(t *testing.T) {
	main := mustRead(t, "../../cmd/manyforge/main.go")
	ingress := mf017Slice(t, main, "ingress.Use(h.ingestLimit)", "})")
	if strings.Contains(ingress, "mailingWebhook") {
		t.Error("the mailing provider webhook is mounted inside the ingestLimit group again")
	}
	provider := mf017Slice(t, main, "provider.Use(h.mailingHookLimit)", "})")
	if !strings.Contains(provider, "h.mailingWebhook.PublicRoutes(provider)") {
		t.Error("the mailing provider webhook must be mounted behind h.mailingHookLimit")
	}
	for _, pin := range []string{
		"ratelimit.NewTokenBucket(cfg.MailingWebhookIPRateRPS, cfg.MailingWebhookIPRateBurst)",
		"mailingWebhookH.ProfileLimiter = ratelimit.NewTokenBucket(cfg.MailingWebhookProfileRateRPS, cfg.MailingWebhookProfileRateBurst)",
		"mailingHookLimit: mailingWebhookHTTP",
	} {
		if !strings.Contains(main, pin) {
			t.Errorf("main.go missing %q", pin)
		}
	}
}

func TestPin_SecreviewM1ProfileBudgetAfterSignature(t *testing.T) {
	resend := mustRead(t, "../mailing/webhook_resend.go")
	handle := mf017Slice(t, resend, "func (h *WebhookHandler) handleResend(", "\n}\n")
	verify, allow := strings.Index(handle, "verifySvix("), strings.Index(handle, "h.allowProfile(w, profileID)")
	if verify < 0 || allow < 0 || allow < verify {
		t.Error("handleResend must spend the per-profile budget only after verifySvix succeeds")
	}
	ses := mustRead(t, "../mailing/webhook_ses.go")
	handle = mf017Slice(t, ses, "func (h *WebhookHandler) handleSES(", "\n}\n")
	verify, allow = strings.Index(handle, "h.SNS.Verify("), strings.Index(handle, "h.allowProfile(w, profileID)")
	if verify < 0 || allow < 0 || allow < verify {
		t.Error("handleSES must spend the per-profile budget only after SNS verification succeeds")
	}
}

func TestSecreviewM1ConfigDefaultsAreGenerousAndValidated(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MailingWebhookIPRateRPS < 50 || cfg.MailingWebhookIPRateBurst < 200 ||
		cfg.MailingWebhookProfileRateRPS < 20 || cfg.MailingWebhookProfileRateBurst < 200 {
		t.Errorf("mailing webhook limits too low: ip=%v/%v profile=%v/%v",
			cfg.MailingWebhookIPRateRPS, cfg.MailingWebhookIPRateBurst,
			cfg.MailingWebhookProfileRateRPS, cfg.MailingWebhookProfileRateBurst)
	}
	for _, key := range []string{
		"MANYFORGE_MAILING_WEBHOOK_IP_RATE_RPS", "MANYFORGE_MAILING_WEBHOOK_IP_RATE_BURST",
		"MANYFORGE_MAILING_WEBHOOK_PROFILE_RATE_RPS", "MANYFORGE_MAILING_WEBHOOK_PROFILE_RATE_BURST",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "0")
			if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("%s=0: Load error = %v, want a validation error", key, err)
			}
		})
	}
}

// Unsigned requests never reach the per-profile budget: they are rejected before
// it, so they are bounded only by the pre-auth ceiling (route test in cmd/manyforge).
func TestSecreviewM1UnsignedRequestsDoNotSpendProfileBudget(t *testing.T) {
	h := mailing.NewWebhookHandler(nil, nil, nil)
	h.ProfileLimiter = ratelimit.NewTokenBucket(0.0001, 1)
	router := chi.NewRouter()
	h.PublicRoutes(router)
	profile := uuid.NewString()
	for i := 0; i < 5; i++ {
		for _, kind := range []string{"resend", "ses"} {
			req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+profile+"/"+kind, strings.NewReader(`{}`))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("unsigned %s request %d status = %d, want 401 (never the per-profile 429)", kind, i, w.Code)
			}
		}
	}
}
