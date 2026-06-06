package inbox

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// recipientEnvelope marshals an InboundEmailEnvelope addressed to a specific
// recipient so the per-recipient limiter can be keyed on the decoded address.
func recipientEnvelope(t *testing.T, to string) []byte {
	t.Helper()
	env := map[string]any{
		"to":         []string{to},
		"from":       "alice@example.com",
		"subject":    "hello",
		"message_id": uuid.NewString() + "@example.com",
		"body_text":  "hi there",
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

// newRateLimitedHandler mounts the webhook handler with a per-recipient ingest
// limiter, exactly as production wires it (the per-IP limiter is a middleware in
// main.go, exercised separately below).
func newRateLimitedHandler(t *testing.T, ing Ingester, limiter ratelimit.Limiter) http.Handler {
	t.Helper()
	cfg := Config{InboundSystemDomain: "inbound.localhost"}
	h := NewWebhookHandler(ing, testWebhookSecret, 1<<20, cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.SetRecipientLimiter(limiter)
	r := chi.NewRouter()
	h.PublicRoutes(r)
	return r
}

// TestWebhookPerRecipientRateLimit drives the SAME recipient over its per-recipient
// cap and asserts the over-cap request is throttled with 429 and ingestion is NOT
// reached, while requests under the cap still ingest (202).
func TestWebhookPerRecipientRateLimit(t *testing.T) {
	const burst = 2
	// rate 0 so no tokens refill within the test window: exactly `burst` allowed.
	limiter := ratelimit.NewTokenBucket(0, burst)
	ing := &fakeIngester{result: IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Created: true}}
	handler := newRateLimitedHandler(t, ing, limiter)

	const rcpt = "support@inbound.localhost"

	// First `burst` requests for the recipient must pass through to Ingest (202).
	accepted := 0
	for i := 0; i < burst; i++ {
		body := recipientEnvelope(t, rcpt)
		sig := signBody(testWebhookSecret, body)
		rec := doRequest(handler, "postmark", body, sig)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("request %d: want 202 under cap, got %d (body %q)", i, rec.Code, rec.Body.String())
		}
		accepted++
	}
	if ing.calls != burst {
		t.Fatalf("Ingest called %d times under cap, want %d", ing.calls, burst)
	}

	// The next request for the SAME recipient is over the cap: 429, and Ingest must
	// NOT be invoked (the throttle short-circuits before resolution).
	body := recipientEnvelope(t, rcpt)
	sig := signBody(testWebhookSecret, body)
	rec := doRequest(handler, "postmark", body, sig)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over cap: want 429, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if ing.calls != burst {
		t.Errorf("Ingest was called %d times; the over-cap request must NOT reach ingestion (want %d)", ing.calls, burst)
	}
}

// TestWebhookPerRecipientRateLimitNoOracle is the critical no-oracle property: a
// KNOWN-routing recipient and an UNKNOWN recipient must hit the per-recipient
// throttle IDENTICALLY (same status AND byte-identical body at the same request
// count). The throttle is applied on the decoded recipient string BEFORE recipient
// resolution, so the 429 cannot reveal whether the recipient routes.
func TestWebhookPerRecipientRateLimitNoOracle(t *testing.T) {
	const burst = 1
	const knownRcpt = "support@inbound.localhost"
	const unknownRcpt = "nobody@inbound.localhost"

	// drive runs `burst` accepted requests then one over-cap request for a single
	// recipient, returning the over-cap (status, body). The ingester is configured
	// so the recipient either routes (Created) or is unknown (errNoRoute) — but the
	// throttle short-circuits BEFORE Ingest, so that distinction must not leak.
	drive := func(t *testing.T, rcpt string, ingErr error) (int, []byte) {
		t.Helper()
		var result IngestResult
		if ingErr == nil {
			result = IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Created: true}
		}
		ing := &fakeIngester{result: result, err: ingErr}
		limiter := ratelimit.NewTokenBucket(0, burst)
		handler := newRateLimitedHandler(t, ing, limiter)

		for i := 0; i < burst; i++ {
			body := recipientEnvelope(t, rcpt)
			rec := doRequest(handler, "postmark", body, signBody(testWebhookSecret, body))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("%s req %d: want 202 under cap, got %d", rcpt, i, rec.Code)
			}
		}
		body := recipientEnvelope(t, rcpt)
		rec := doRequest(handler, "postmark", body, signBody(testWebhookSecret, body))
		return rec.Code, rec.Body.Bytes()
	}

	knownStatus, knownBody := drive(t, knownRcpt, nil)              // routes
	unknownStatus, unknownBody := drive(t, unknownRcpt, errNoRoute) // does not route

	if knownStatus != http.StatusTooManyRequests {
		t.Fatalf("known recipient over cap: want 429, got %d", knownStatus)
	}
	if unknownStatus != http.StatusTooManyRequests {
		t.Fatalf("unknown recipient over cap: want 429, got %d", unknownStatus)
	}
	if knownStatus != unknownStatus {
		t.Errorf("per-recipient throttle is an oracle: known status %d != unknown status %d", knownStatus, unknownStatus)
	}
	if !bytes.Equal(knownBody, unknownBody) {
		t.Errorf("per-recipient 429 body is an oracle:\n known=%q\n unknown=%q", knownBody, unknownBody)
	}
}

// TestWebhookNoRecipientLimiterUnchanged confirms that with NO per-recipient limiter
// configured (nil), the uniform-202 behavior is unchanged: routed, duplicate, and
// unknown-recipient all still 202 (the rate limit is an ADDITIVE layer).
func TestWebhookNoRecipientLimiterUnchanged(t *testing.T) {
	cases := []struct {
		name string
		ing  *fakeIngester
	}{
		{"routed", &fakeIngester{result: IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Created: true}}},
		{"duplicate", &fakeIngester{result: IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Duplicate: true}}},
		{"unknown recipient", &fakeIngester{err: errNoRoute}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			handler := newRateLimitedHandler(t, c.ing, nil) // nil limiter ⇒ no per-recipient cap
			body := recipientEnvelope(t, "support@inbound.localhost")
			rec := doRequest(handler, "postmark", body, signBody(testWebhookSecret, body))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("%s: want 202 with no limiter, got %d (body %q)", c.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestWebhookPerIPRateLimitMiddleware exercises the per-IP limiter exactly as
// main.go wraps the webhook route group: requests from one IP over the cap get 429
// (from the middleware), and once throttled the handler/ingester is never reached.
func TestWebhookPerIPRateLimitMiddleware(t *testing.T) {
	const burst = 2
	ing := &fakeIngester{result: IngestResult{TicketID: uuid.New(), MessageID: uuid.New(), Created: true}}
	cfg := Config{InboundSystemDomain: "inbound.localhost"}
	h := NewWebhookHandler(ing, testWebhookSecret, 1<<20, cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	ipLimiter := ratelimit.NewTokenBucket(0, burst)
	ipKey := func(r *http.Request) string { return ratelimit.ClientIP(r, nil) }

	r := chi.NewRouter()
	r.Group(func(g chi.Router) {
		g.Use(httpx.RateLimit(ipLimiter, ipKey))
		h.PublicRoutes(g)
	})

	body := recipientEnvelope(t, "support@inbound.localhost")
	sig := signBody(testWebhookSecret, body)
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/inbound/email/postmark", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-MF-Signature", sig)
		req.RemoteAddr = "198.51.100.9:4444" // single source IP
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	for i := 0; i < burst; i++ {
		if rec := send(); rec.Code != http.StatusAccepted {
			t.Fatalf("under per-IP cap req %d: want 202, got %d", i, rec.Code)
		}
	}
	before := ing.calls
	if rec := send(); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over per-IP cap: want 429, got %d", rec.Code)
	}
	if ing.calls != before {
		t.Errorf("per-IP throttle let a request through to Ingest: calls %d, want %d", ing.calls, before)
	}
}
