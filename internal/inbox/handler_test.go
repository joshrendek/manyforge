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

	"github.com/manyforge/manyforge/internal/platform/observability"
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
	// got captures every RawMessage handed to Ingest so handler-level decode
	// behavior (e.g. envelope-recipient extraction) can be asserted.
	got []RawMessage
}

func (f *fakeIngester) Ingest(_ context.Context, msg RawMessage) (IngestResult, error) {
	f.called = true
	f.calls++
	f.got = append(f.got, msg)
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

// doRFC822Request issues a signed POST with a bare message/rfc822 body (no JSON
// envelope), exercising the raw-MIME decode path.
func doRFC822Request(handler http.Handler, provider string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/inbound/email/"+provider, bytes.NewReader(body))
	req.Header.Set("Content-Type", "message/rfc822")
	req.Header.Set("X-MF-Signature", signBody(testWebhookSecret, body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestWebhookRFC822EnvelopeRecipientFromTo asserts that a bare message/rfc822
// webhook delivery (no JSON envelope to carry routing) has its EnvelopeRecipient
// populated best-effort from the message's To header. Without this, Service.Ingest
// — which routes ONLY on EnvelopeRecipient — would no-route and silently drop a
// delivery the OpenAPI advertises as supported (message/rfc822). security:
// MF-002-INGEST-SCOPE — no-oracle is preserved: resolve_inbound_address still gates
// on the business's actual address, so an unknown/unresolving To still yields 202.
func TestWebhookRFC822EnvelopeRecipientFromTo(t *testing.T) {
	const wantRcpt = "support@inbound.localhost"
	raw := []byte("From: alice@example.com\r\n" +
		"To: " + wantRcpt + "\r\n" +
		"Subject: bare mime\r\n" +
		"Message-ID: <bare-1@example.com>\r\n" +
		"\r\n" +
		"hello from raw mime\r\n")

	ing := &fakeIngester{result: IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Created: true}}
	rec := doRFC822Request(newTestHandler(t, ing), "smtp", raw)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if len(ing.got) != 1 {
		t.Fatalf("Ingest called %d times, want 1", len(ing.got))
	}
	if got := ing.got[0].EnvelopeRecipient; got != wantRcpt {
		t.Errorf("EnvelopeRecipient = %q, want %q (extracted from To header)", got, wantRcpt)
	}
	// The raw bytes must still flow through untouched for downstream parsing.
	if !bytes.Equal(ing.got[0].Raw, raw) {
		t.Errorf("Raw bytes were altered; want the original message body verbatim")
	}
}

// TestWebhookIncrementsMetrics verifies that the Handler increments the
// observability counters correctly: received on every call, rejected on auth
// failure, accepted on a routed ingest.
func TestWebhookIncrementsMetrics(t *testing.T) {
	cfg := Config{InboundSystemDomain: "inbound.localhost"}
	m := observability.NewMetrics()
	base := m.Get(observability.MetricIngestReceived)
	baseAcc := m.Get(observability.MetricIngestAccepted)
	baseRej := m.Get(observability.MetricIngestRejected)
	baseDup := m.Get(observability.MetricIngestDuplicate)

	h := NewWebhookHandler(
		&fakeIngester{result: IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Created: true}},
		testWebhookSecret, 1<<20, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.Metrics = m
	r := chi.NewRouter()
	h.PublicRoutes(r)

	body := envelopeBody(t)
	sig := signBody(testWebhookSecret, body)
	rec := doRequest(r, "postmark", body, sig)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("signed request: status = %d, want 202", rec.Code)
	}

	rec2 := doRequest(r, "postmark", body, "") // no signature → 401
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned request: status = %d, want 401", rec2.Code)
	}

	if got := m.Get(observability.MetricIngestReceived) - base; got != 2 {
		t.Errorf("received delta = %d, want 2", got)
	}
	if got := m.Get(observability.MetricIngestAccepted) - baseAcc; got != 1 {
		t.Errorf("accepted delta = %d, want 1", got)
	}
	if got := m.Get(observability.MetricIngestRejected) - baseRej; got != 1 {
		t.Errorf("rejected delta = %d, want 1", got)
	}
	if got := m.Get(observability.MetricIngestDuplicate) - baseDup; got != 0 {
		t.Errorf("duplicate delta = %d, want 0", got)
	}
}

// TestWebhookRFC822EnvelopeRecipientCcFallback asserts the best-effort extraction
// falls back to the first Cc address when To is absent — so a Cc-only raw delivery
// still routes rather than dropping.
func TestWebhookRFC822EnvelopeRecipientCcFallback(t *testing.T) {
	const wantRcpt = "cc-support@inbound.localhost"
	raw := []byte("From: alice@example.com\r\n" +
		"Cc: " + wantRcpt + "\r\n" +
		"Subject: cc only\r\n" +
		"Message-ID: <bare-2@example.com>\r\n" +
		"\r\n" +
		"cc-only body\r\n")

	ing := &fakeIngester{result: IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Created: true}}
	rec := doRFC822Request(newTestHandler(t, ing), "smtp", raw)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if len(ing.got) != 1 {
		t.Fatalf("Ingest called %d times, want 1", len(ing.got))
	}
	if got := ing.got[0].EnvelopeRecipient; got != wantRcpt {
		t.Errorf("EnvelopeRecipient = %q, want %q (Cc fallback when To absent)", got, wantRcpt)
	}
}
