package mailing

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	mailtoken "github.com/manyforge/manyforge/internal/mailing/token"
)

func TestMailingSignature(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"email":"a@example.test"}`)
	mac := hmac.New(sha256.New, []byte("mls_secret"))
	_, _ = mac.Write(mailingSigningString(ts, http.MethodPost, "/api/v1/mailing/s2s/mlk_x/subscribers", body))
	sig := hex.EncodeToString(mac.Sum(nil))
	if err := verifyMailingSignature(ts, sig, "mls_secret", http.MethodPost, "/api/v1/mailing/s2s/mlk_x/subscribers", body, now); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ timestamp, signature, method string }{
		"expired":  {strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10), sig, http.MethodPost},
		"tampered": {ts, sig[:len(sig)-2] + "00", http.MethodPost},
		"method":   {ts, sig, http.MethodDelete},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyMailingSignature(tc.timestamp, tc.signature, "mls_secret", tc.method, "/api/v1/mailing/s2s/mlk_x/subscribers", body, now); err == nil {
				t.Fatal("invalid signature accepted")
			}
		})
	}
}

func TestUnsubscribeOracleResponsesAreByteIdentical(t *testing.T) {
	codec, err := mailtoken.New(bytes.Repeat([]byte{0x55}, 32))
	if err != nil {
		t.Fatal(err)
	}
	h := NewPublicHandler(&Service{Tokens: codec}, nil, nil, nil)
	r := chi.NewRouter()
	h.RootRoutes(r)

	request := func(method, token string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, "/m/u/"+token, nil))
		return w
	}
	bad := request(http.MethodGet, "bad")
	validUnknown := request(http.MethodGet, codec.EncodeUnsubscribe([16]byte{1}, [16]byte{2}))
	if bad.Code != http.StatusOK || bad.Body.String() != unsubscribePageHTML {
		t.Fatalf("unexpected unsubscribe page: status=%d body=%q", bad.Code, bad.Body.String())
	}
	if bad.Code != validUnknown.Code || bad.Body.String() != validUnknown.Body.String() {
		t.Fatalf("GET oracle differs: bad=(%d,%q), valid unknown=(%d,%q)", bad.Code, bad.Body.String(), validUnknown.Code, validUnknown.Body.String())
	}
}

func TestRootRoutesApplyConfiguredIngressLimit(t *testing.T) {
	called := false
	h := NewPublicHandler(&Service{}, nil, nil, nil)
	h.IngestLimit = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}
	r := chi.NewRouter()
	h.RootRoutes(r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/m/u/token", nil))
	if !called || w.Code != http.StatusTooManyRequests {
		t.Fatalf("ingress limiter called/status = %t/%d", called, w.Code)
	}
}

func TestPublicFormHoneypotIsSilentlyAccepted(t *testing.T) {
	h := NewPublicHandler(nil, nil, nil, nil)
	r := chi.NewRouter()
	h.PublicRoutes(r)
	req := httptest.NewRequest(http.MethodPost, "/mailing/public/mlk_unknown/subscribe", strings.NewReader("email=bot%40example.test&website=https%3A%2F%2Fspam.test"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("honeypot status/body = %d/%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/m/s/mlk_unknown?state=check-inbox" {
		t.Fatalf("honeypot redirect = %q", got)
	}
}
