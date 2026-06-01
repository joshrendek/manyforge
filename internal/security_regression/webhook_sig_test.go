// Finding MF-002-WEBHOOK-SIG (FindingWebhookSig): the provider inbound webhook is
// authenticated ONLY by a per-provider HMAC signature, verified in CONSTANT TIME.
// A tampered body or tampered/absent signature must be rejected with 401 and the
// ingestion boundary must never run. A future refactor that swaps the constant-time
// compare for a `==`/`hmac.Equal`-less path must fail CI.
//
// This file has NO build tag: the behavioral check needs no infrastructure (the
// Ingester boundary is faked), and the source-level pin is a pure string match, so
// both run under `make test` AND `make sec-test`.

package security_regression

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/manyforge/manyforge/internal/inbox"
)

const sigTestSecret = "sec-regression-webhook-secret"

// spyIngester records whether the ingestion boundary was reached. For a rejected
// (401) request it MUST stay false — the signature gate runs before any ingest.
type spyIngester struct{ called bool }

func (s *spyIngester) Ingest(_ context.Context, _ inbox.RawMessage) (inbox.IngestResult, error) {
	s.called = true
	return inbox.IngestResult{Created: true}, nil
}

func signWebhookBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func webhookRequest(t *testing.T, ing inbox.Ingester, body []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	h := inbox.NewWebhookHandler(ing, sigTestSecret, 1<<20, inbox.Config{InboundSystemDomain: "inbound.localhost"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	h.PublicRoutes(r)
	req := httptest.NewRequest(http.MethodPost, "/inbound/email/postmark", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-MF-Signature", sig)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestWebhookSignatureRejectsForgery is the behavioral half of MF-002-WEBHOOK-SIG.
func TestWebhookSignatureRejectsForgery(t *testing.T) {
	body := []byte(`{"to":["support@inbound.localhost"],"from":"a@example.com","message_id":"m1@x","body_text":"hi"}`)

	t.Run("valid signature accepted", func(t *testing.T) {
		ing := &spyIngester{}
		rec := webhookRequest(t, ing, body, signWebhookBody(sigTestSecret, body))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("valid signature: want 202, got %d", rec.Code)
		}
		if !ing.called {
			t.Fatal("valid signature: ingestion boundary was not reached")
		}
	})

	t.Run("tampered body rejected", func(t *testing.T) {
		ing := &spyIngester{}
		sig := signWebhookBody(sigTestSecret, body) // signature over the ORIGINAL body
		tampered := append(bytes.Clone(body[:len(body)-1]), []byte(`,"x":1}`)...)
		rec := webhookRequest(t, ing, tampered, sig)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("tampered body: want 401, got %d", rec.Code)
		}
		if ing.called {
			t.Fatal("tampered body: ingestion boundary MUST NOT be reached")
		}
	})

	t.Run("tampered signature rejected", func(t *testing.T) {
		ing := &spyIngester{}
		rec := webhookRequest(t, ing, body, signWebhookBody("wrong-secret", body))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("tampered signature: want 401, got %d", rec.Code)
		}
		if ing.called {
			t.Fatal("tampered signature: ingestion boundary MUST NOT be reached")
		}
	})

	t.Run("absent signature rejected", func(t *testing.T) {
		ing := &spyIngester{}
		rec := webhookRequest(t, ing, body, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("absent signature: want 401, got %d", rec.Code)
		}
		if ing.called {
			t.Fatal("absent signature: ingestion boundary MUST NOT be reached")
		}
	})
}

// TestWebhookConstantTimeComparePinned is the source-level half: the constant-time
// compare must remain in the SHARED inbound-webhook HMAC verifier so a refactor to
// `==` (timing oracle) fails CI loudly. The verify body was extracted into the
// package-shared inbox.verifyHMAC (internal/inbox/signature.go) — the single source
// of truth that BOTH the inbound-email webhook (WebhookAdapter.verify) and the
// hard-bounce webhook (BounceHandler.verify) delegate to, so neither can drift to a
// variable-time compare independently. Finding ID: FindingWebhookSig.
func TestWebhookConstantTimeComparePinned(t *testing.T) {
	src := mustRead(t, "../inbox/signature.go")
	if !strings.Contains(src, "subtle.ConstantTimeCompare") {
		t.Errorf("%s: constant-time compare pin missing from internal/inbox/signature.go "+
			"(shared verifyHMAC) — was the HMAC verify weakened to a variable-time `==`?", FindingWebhookSig)
	}
}
