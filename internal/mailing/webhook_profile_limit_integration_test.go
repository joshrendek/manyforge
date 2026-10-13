//go:build integration

package mailing_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// SECREVIEW-M1: two sending profiles behind ONE provider egress IP each get their
// own post-auth budget. A flooding profile is throttled without 429ing the other,
// and unsigned junk for a profile never spends that profile's budget.
func TestResendWebhookPerProfileBudgetBehindSharedIP(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	fxA := seedWebhookFixture(ctx, t, tdb, seedMailingTenant(ctx, t, tdb))
	fxB := seedWebhookFixture(ctx, t, tdb, seedMailingTenant(ctx, t, tdb))

	const profileBurst = 3
	h := mailing.NewWebhookHandler(tdb.App, fxA.svc.Sealer, nil)
	now := time.Unix(1_800_000_000, 0).UTC()
	h.Now = func() time.Time { return now }
	h.ProfileLimiter = ratelimit.NewTokenBucket(0.0001, profileBurst)
	router := chi.NewRouter()
	// Production shape: a generous pre-auth per-IP ceiling in front.
	router.Use(httpx.RateLimit(ratelimit.NewTokenBucket(0.0001, 1000),
		func(r *http.Request) string { return ratelimit.ClientIP(r, nil) }))
	h.PublicRoutes(router)
	webhookKey := bytes.Repeat([]byte{0x51}, 32)

	post := func(profileID uuid.UUID, eventID string, signed bool) *httptest.ResponseRecorder {
		t.Helper()
		body := []byte(`{"type":"email.delivery_delayed","created_at":"2026-08-30T12:00:00Z","data":{"email_id":"x","to":["a@example.test"]}}`)
		ts := fmt.Sprint(now.Unix())
		sig := "v1,AAAA"
		if signed {
			mac := hmac.New(sha256.New, webhookKey)
			mac.Write([]byte(eventID + "." + ts + "."))
			mac.Write(body)
			sig = "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		}
		req := httptest.NewRequest(http.MethodPost, "/inbound/mailing/"+profileID.String()+"/resend", bytes.NewReader(body))
		req.RemoteAddr = "198.51.100.7:443" // one shared Resend egress IP
		req.Header.Set("svix-id", eventID)
		req.Header.Set("svix-timestamp", ts)
		req.Header.Set("svix-signature", sig)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// Unsigned junk for B is rejected and does not drain B's post-auth budget.
	for i := 0; i < 2*profileBurst; i++ {
		if w := post(fxB.profileID, fmt.Sprintf("junk-%d", i), false); w.Code != http.StatusUnauthorized {
			t.Fatalf("unsigned request %d status = %d, want 401", i, w.Code)
		}
	}
	// Profile A floods past its budget.
	for i := 0; i < profileBurst; i++ {
		if w := post(fxA.profileID, fmt.Sprintf("a-%d", i), true); w.Code != http.StatusOK {
			t.Fatalf("profile A request %d status = %d body=%s", i, w.Code, w.Body.String())
		}
	}
	w := post(fxA.profileID, "a-over", true)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "RATE_LIMITED") {
		t.Fatalf("profile A over budget status = %d body=%s, want 429 RATE_LIMITED", w.Code, w.Body.String())
	}
	// Profile B, same client IP, still has its full budget.
	for i := 0; i < profileBurst; i++ {
		if w := post(fxB.profileID, fmt.Sprintf("b-%d", i), true); w.Code != http.StatusOK {
			t.Fatalf("profile B request %d status = %d body=%s (throttled by A's flood?)", i, w.Code, w.Body.String())
		}
	}
}
