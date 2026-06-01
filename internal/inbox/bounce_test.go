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
	"testing"

	"github.com/go-chi/chi/v5"
)

const testBounceSecret = "test-bounce-secret"

// fakeSuppressor records each (recipient, messageID) the handler asked to suppress
// + mark-failed, so the handler-level contract (one suppression per valid hard
// bounce; never for soft/parse-fail/bad-sig) can be asserted without a database.
type fakeSuppressor struct {
	calls []suppressCall
	err   error
}

type suppressCall struct {
	recipient string
	messageID string
}

func (f *fakeSuppressor) SuppressBounce(_ context.Context, recipient, messageID string) error {
	f.calls = append(f.calls, suppressCall{recipient: recipient, messageID: messageID})
	return f.err
}

// signBounce produces the canonical X-MF-Signature value (hex HMAC-SHA256 over the
// raw body) the bounce handler verifies — identical scheme to the inbox webhook.
func signBounce(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// bounceBody marshals a bounce payload for the webhook body.
func bounceBody(t *testing.T, recipient, typ, messageID string) []byte {
	t.Helper()
	m := map[string]any{"recipient": recipient, "type": typ}
	if messageID != "" {
		m["message_id"] = messageID
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal bounce: %v", err)
	}
	return raw
}

// newBounceHandler mounts the bounce handler on a chi router exactly as production
// does, so the route plumbing is exercised.
func newBounceHandler(t *testing.T, sup BounceSuppressor) http.Handler {
	t.Helper()
	h := NewBounceHandler(sup, testBounceSecret, 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	h.PublicRoutes(r)
	return r
}

// doBounce issues a POST to the bounce route with the given (optional) signature.
func doBounce(handler http.Handler, body []byte, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/inbound/bounce", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-MF-Signature", sig)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestBounceValidHardBounce202 — a valid-HMAC hard bounce with a message_id: 202,
// and exactly one SuppressBounce(recipient, messageID) call.
func TestBounceValidHardBounce202(t *testing.T) {
	body := bounceBody(t, "bounced@example.com", "hard", "msg-1@inbound.localhost")
	sup := &fakeSuppressor{}
	rec := doBounce(newBounceHandler(t, sup), body, signBounce(testBounceSecret, body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid hard bounce: want 202, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if len(sup.calls) != 1 {
		t.Fatalf("SuppressBounce called %d times, want 1", len(sup.calls))
	}
	if got := sup.calls[0]; got.recipient != "bounced@example.com" || got.messageID != "msg-1@inbound.localhost" {
		t.Errorf("SuppressBounce got %+v, want {bounced@example.com msg-1@inbound.localhost}", got)
	}
}

// TestBounceBadSignature401 — a missing or forged signature is an AUTHENTICATION
// failure ⇒ 401, EXACTLY like the inbox webhook (handler.go). A 401 on a bad HMAC is
// not a recipient oracle: an attacker without the secret already knows they cannot
// forge. The suppression path is NEVER reached.
func TestBounceBadSignature401(t *testing.T) {
	hard := bounceBody(t, "bounced@example.com", "hard", "msg-1@inbound.localhost")
	cases := []struct {
		name string
		sig  string
	}{
		{"missing signature", ""},
		{"tampered signature", signBounce(testBounceSecret, append([]byte("x"), hard...))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sup := &fakeSuppressor{}
			rec := doBounce(newBounceHandler(t, sup), hard, c.sig)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("want 401, got %d (body %q)", rec.Code, rec.Body.String())
			}
			if len(sup.calls) != 0 {
				t.Errorf("SuppressBounce MUST NOT run on an unauthenticated request; got %d calls", len(sup.calls))
			}
		})
	}
}

// TestBounceUniform202 — the real no-oracle property: among AUTHENTICATED (valid-HMAC)
// requests, a soft bounce, an unparseable body, and a valid hard bounce whose
// recipient/message does NOT correlate to anything ALL return the SAME 202 as a
// matching hard bounce, byte-identical. An authenticated caller therefore cannot probe
// which recipients are real customers or which Message-IDs exist. The unparseable-body
// and soft-bounce cases additionally must not touch the suppression path; the
// no-match case DOES call SuppressBounce (suppression is unconditional on a hard
// bounce — the no-oracle guarantee is about the RESPONSE, not whether a DB row
// matched, which the handler cannot and must not observe).
func TestBounceUniform202(t *testing.T) {
	hard := bounceBody(t, "bounced@example.com", "hard", "msg-1@inbound.localhost")

	// Reference: a matching hard-bounce 202 (status + body) every other authenticated
	// outcome must be byte-identical to.
	ref := doBounce(newBounceHandler(t, &fakeSuppressor{}), hard, signBounce(testBounceSecret, hard))
	if ref.Code != http.StatusAccepted {
		t.Fatalf("reference hard bounce: want 202, got %d", ref.Code)
	}
	refBody := ref.Body.Bytes()

	cases := []struct {
		name         string
		body         []byte
		wantSuppress bool
	}{
		{"soft bounce", bounceBody(t, "bounced@example.com", "soft", "msg-1@inbound.localhost"), false},
		{"unparseable body", []byte("{not json"), false},
		// A VALID-HMAC hard bounce whose recipient/message matches NOTHING: the handler
		// still suppresses (it cannot know there is no match — that's the point: the
		// response is identical whether or not a DB row existed). This is the canonical
		// authenticated-but-no-match → 202 no-oracle case.
		{"authenticated no-match", bounceBody(t, "stranger@nowhere.invalid", "hard", "unknown-99@inbound.localhost"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sup := &fakeSuppressor{}
			// Every case here is AUTHENTICATED — sign the body with the real secret.
			rec := doBounce(newBounceHandler(t, sup), c.body, signBounce(testBounceSecret, c.body))
			if rec.Code != ref.Code {
				t.Errorf("status: want %d (identical to matching hard bounce), got %d", ref.Code, rec.Code)
			}
			if !bytes.Equal(rec.Body.Bytes(), refBody) {
				t.Errorf("body not byte-identical to the matching-bounce 202 (oracle!):\n ref=%q\n got=%q", refBody, rec.Body.Bytes())
			}
			if c.wantSuppress && len(sup.calls) != 1 {
				t.Errorf("want 1 SuppressBounce call, got %d", len(sup.calls))
			}
			if !c.wantSuppress && len(sup.calls) != 0 {
				t.Errorf("want 0 SuppressBounce calls (no suppression for %s), got %d", c.name, len(sup.calls))
			}
		})
	}
}

// TestBounceBodyTooLarge202 — a body over the cap is refused before the signature
// is computed, and still yields the uniform 202 with NO suppression (no oracle: an
// oversized body discloses nothing and never reaches the suppression path).
func TestBounceBodyTooLarge202(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 4096)
	sup := &fakeSuppressor{}
	h := NewBounceHandler(sup, testBounceSecret, 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	h.PublicRoutes(r)

	rec := doBounce(r, big, signBounce(testBounceSecret, big))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("oversized body: want uniform 202, got %d (body %q)", rec.Code, rec.Body.String())
	}
	if len(sup.calls) != 0 {
		t.Errorf("oversized body must not reach suppression; got %d calls", len(sup.calls))
	}
}

// TestBounceSha256Prefix202 — the "sha256=" prefix branch of verify is exercised:
// a prefixed valid signature is accepted and the hard bounce is suppressed.
func TestBounceSha256Prefix202(t *testing.T) {
	body := bounceBody(t, "bounced@example.com", "hard", "msg-1@inbound.localhost")
	sup := &fakeSuppressor{}
	rec := doBounce(newBounceHandler(t, sup), body, "sha256="+signBounce(testBounceSecret, body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("sha256= prefixed signature: want 202, got %d", rec.Code)
	}
	if len(sup.calls) != 1 {
		t.Errorf("sha256= prefixed valid signature: want 1 suppress call, got %d", len(sup.calls))
	}
}
