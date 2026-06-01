package inbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// fakeIngester is a test double for the Ingester boundary: the handler-level
// contract is exercised without a database. It records whether it was called and
// returns a scripted result/error so the no-oracle and error-mapping properties
// can be asserted in isolation.
type fakeIngester struct {
	called bool
	calls  int
	result IngestResult
	err    error
}

func (f *fakeIngester) Ingest(_ context.Context, _ RawMessage) (IngestResult, error) {
	f.called = true
	f.calls++
	return f.result, f.err
}

const testWebhookSecret = "test-inbound-secret"

// signBody produces the canonical X-MF-Signature value (hex HMAC-SHA256 over the
// raw body) the handler verifies.
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// envelopeBody marshals a minimal valid InboundEmailEnvelope for the webhook body.
func envelopeBody(t *testing.T) []byte {
	t.Helper()
	env := map[string]any{
		"to":         []string{"support@inbound.localhost"},
		"from":       "alice@example.com",
		"subject":    "hello",
		"message_id": "msg-1@example.com",
		"body_text":  "hi there",
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

// newTestHandler mounts the webhook handler exactly as production does, on a chi
// router with the {provider} path param, so the param plumbing is exercised.
func newTestHandler(t *testing.T, ing Ingester) http.Handler {
	t.Helper()
	cfg := Config{InboundSystemDomain: "inbound.localhost"}
	h := NewWebhookHandler(ing, testWebhookSecret, 1<<20, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	h.PublicRoutes(r)
	return r
}

// doRequest issues a signed (or unsigned) POST to the webhook route.
func doRequest(handler http.Handler, provider string, body []byte, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/inbound/email/"+provider, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-MF-Signature", sig)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestWebhookUniform202(t *testing.T) {
	body := envelopeBody(t)
	sig := signBody(testWebhookSecret, body)

	// Capture the routed (created) response as the reference: the no-oracle
	// property requires unknown-recipient and duplicate to be BYTE-IDENTICAL.
	routed := &fakeIngester{result: IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Created: true}}
	routedRec := doRequest(newTestHandler(t, routed), "postmark", body, sig)
	if routedRec.Code != http.StatusAccepted {
		t.Fatalf("routed: want 202, got %d (body %q)", routedRec.Code, routedRec.Body.String())
	}
	if !routed.called {
		t.Fatal("routed: Ingest was not called")
	}
	refStatus := routedRec.Code
	refBody := routedRec.Body.Bytes()

	cases := []struct {
		name string
		ing  *fakeIngester
	}{
		{"unknown recipient (errNoRoute)", &fakeIngester{err: errNoRoute}},
		{"duplicate message-id", &fakeIngester{result: IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Duplicate: true}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doRequest(newTestHandler(t, c.ing), "postmark", body, sig)
			if !c.ing.called {
				t.Fatal("Ingest was not called")
			}
			if rec.Code != refStatus {
				t.Errorf("status: want %d (identical to routed), got %d", refStatus, rec.Code)
			}
			if !bytes.Equal(rec.Body.Bytes(), refBody) {
				t.Errorf("body not byte-identical to routed case (oracle!):\n routed=%q\n  this=%q", refBody, rec.Body.Bytes())
			}
		})
	}
}

// TestWebhookSha256PrefixAccepted exercises the "sha256=" prefix branch of
// verify(): some providers prefix the hex digest with "sha256=". The adapter strips
// it and the signature is accepted (202), and ingestion runs. Without this test the
// prefix-strip is untested dead code.
func TestWebhookSha256PrefixAccepted(t *testing.T) {
	body := envelopeBody(t)
	sig := "sha256=" + signBody(testWebhookSecret, body)
	ing := &fakeIngester{result: IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Created: true}}
	rec := doRequest(newTestHandler(t, ing), "postmark", body, sig)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("sha256= prefixed signature: want 202, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if !ing.called {
		t.Error("sha256= prefixed signature: Ingest should have been called (signature is valid)")
	}
}

func TestWebhookMissingSignature401(t *testing.T) {
	body := envelopeBody(t)
	ing := &fakeIngester{result: IngestResult{Created: true}}
	rec := doRequest(newTestHandler(t, ing), "postmark", body, "") // no signature header
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if ing.called {
		t.Error("Ingest MUST NOT be called when the signature is missing")
	}
}

func TestWebhookTamperedSignature401(t *testing.T) {
	body := envelopeBody(t)
	ing := &fakeIngester{result: IngestResult{Created: true}}
	// A valid-looking but wrong signature (signed over different bytes).
	badSig := signBody(testWebhookSecret, append([]byte("tampered"), body...))
	rec := doRequest(newTestHandler(t, ing), "postmark", body, badSig)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if ing.called {
		t.Error("Ingest MUST NOT be called when the signature is invalid")
	}
}

func TestWebhookBodyTooLarge413(t *testing.T) {
	// Build a body larger than the 1 KiB cap configured below.
	big := bytes.Repeat([]byte("a"), 4096)
	cfg := Config{InboundSystemDomain: "inbound.localhost"}
	ing := &fakeIngester{result: IngestResult{Created: true}}
	h := NewWebhookHandler(ing, testWebhookSecret, 1024, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	h.PublicRoutes(r)

	sig := signBody(testWebhookSecret, big) // signature valid; the cap must still trip first
	rec := doRequest(r, "postmark", big, sig)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if ing.called {
		t.Error("Ingest MUST NOT be called when the body exceeds the cap")
	}
}

func TestWebhookInternalError500Generic(t *testing.T) {
	body := envelopeBody(t)
	sig := signBody(testWebhookSecret, body)
	const secretLeak = "pq: relation \"ticket\" violates constraint reply_token_unique"
	ing := &fakeIngester{err: &leakyError{msg: secretLeak}}
	rec := doRequest(newTestHandler(t, ing), "postmark", body, sig)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretLeak) {
		t.Errorf("500 body leaked the internal error string: %q", rec.Body.String())
	}
	if !ing.called {
		t.Error("Ingest should have been called before the internal error")
	}
}

// leakyError carries a sensitive internal message that must never reach the client.
type leakyError struct{ msg string }

func (e *leakyError) Error() string { return e.msg }
