package mailing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// SECREVIEW-M1: the post-auth budget is per sending profile, so one flooded
// profile cannot 429 another profile sharing the same provider egress IP.
func TestAllowProfileBudgetsAreIndependentPerProfile(t *testing.T) {
	h := &WebhookHandler{ProfileLimiter: ratelimit.NewTokenBucket(0.0001, 2)}
	a, b := uuid.New(), uuid.New()
	for i := 0; i < 2; i++ {
		if !h.allowProfile(httptest.NewRecorder(), a) {
			t.Fatalf("profile A request %d throttled inside its burst", i)
		}
	}
	w := httptest.NewRecorder()
	if h.allowProfile(w, a) {
		t.Fatal("profile A allowed past its budget")
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" ||
		!strings.Contains(w.Body.String(), `"RATE_LIMITED"`) {
		t.Fatalf("over-budget response = %d %q %s; want 429 RATE_LIMITED with Retry-After", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if !h.allowProfile(httptest.NewRecorder(), b) {
		t.Fatal("profile B throttled by profile A's flood")
	}
}

func TestAllowProfileNilLimiterAllows(t *testing.T) {
	if !(&WebhookHandler{}).allowProfile(httptest.NewRecorder(), uuid.New()) {
		t.Fatal("nil ProfileLimiter must not throttle")
	}
}
