//go:build integration

package mailing_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	mailrender "github.com/manyforge/manyforge/internal/mailing/render"
	mailtoken "github.com/manyforge/manyforge/internal/mailing/token"
	mfcrypto "github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

type toggleLimiter struct {
	deny bool
}

func (l *toggleLimiter) Allow(string) bool {
	return !l.deny
}

func TestPublicDoubleOptInConfirmUnsubscribeAndS2S(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	master := bytes.Repeat([]byte{0x61}, 32)
	sealer, err := mfcrypto.NewSealer(master)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := mailtoken.New(master)
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := mailrender.New()
	if err != nil {
		t.Fatal(err)
	}
	captured := &capturedDeliverer{}
	svc := &mailing.Service{
		DB: tdb.App, Sealer: sealer, Vault: secrets.NewVault(sealer), Tokens: codec,
		Providers: mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
			return captured, nil
		}, time.Minute), Renderer: renderer, MessageDomain: "mail.example.test",
		PublicBaseURL: "https://hub.example.test",
		Now:           func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	list, err := svc.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Updates", DoubleOptIn: true})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := svc.PutSendingProfile(ctx, seed.principalID, seed.businessID, mailing.SendingProfileInput{
		Mode: "resend", FromEmail: "updates@example.test", FromName: "Updates",
		Resend: &mailing.ResendCredentials{APIKey: "re_test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE mailing_sending_profile SET status='verified' WHERE id=$1", profile.ID); err != nil {
		t.Fatal(err)
	}
	key, err := svc.CreateListKey(ctx, seed.principalID, seed.businessID, list.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := mailing.NewPublicHandler(svc, nil, sealer, func(*http.Request) string { return "203.0.113.7" })
	h.Now = svc.Now
	perKey := &toggleLimiter{}
	h.PerKey = perKey
	router := chi.NewRouter()
	router.Route("/api/v1", h.PublicRoutes)
	h.RootRoutes(router)

	request := func(method, target string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, target, bytes.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	subscribeBody := []byte(`{"email":"Ada@Example.test","first_name":"Ada"}`)
	accepted := request(http.MethodPost, "/api/v1/mailing/public/"+key.PublishableKey+"/subscribe", subscribeBody, map[string]string{"Content-Type": "application/json", "User-Agent": "mailing-test/1"})
	if accepted.Code != http.StatusAccepted || accepted.Body.String() != "{\"accepted\":true}\n" {
		t.Fatalf("subscribe status/body = %d/%s", accepted.Code, accepted.Body.String())
	}
	unknown := request(http.MethodPost, "/api/v1/mailing/public/mlk_unknown/subscribe", subscribeBody, map[string]string{"Content-Type": "application/json"})
	if unknown.Code != accepted.Code || unknown.Body.String() != accepted.Body.String() {
		t.Fatalf("public key oracle differs: known=(%d,%q), unknown=(%d,%q)", accepted.Code, accepted.Body.String(), unknown.Code, unknown.Body.String())
	}

	var subscriberID uuid.UUID
	var status, consentIP, userAgent string
	var storedHash []byte
	var expires time.Time
	if err := tdb.Super.QueryRow(ctx, `SELECT id,status::text,consent_ip::text,consent_user_agent,confirm_token_hash,confirm_expires_at FROM list_subscriber WHERE list_id=$1 AND email=$2`, list.ID, "ada@example.test").Scan(&subscriberID, &status, &consentIP, &userAgent, &storedHash, &expires); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || consentIP != "203.0.113.7/32" || userAgent != "mailing-test/1" || len(storedHash) != sha256.Size {
		t.Fatalf("pending subscriber status/ip/ua/hash = %q/%q/%q/%x", status, consentIP, userAgent, storedHash)
	}
	if want := svc.Now().Add(48 * time.Hour); !expires.Equal(want) {
		t.Fatalf("confirmation expiry = %s, want %s", expires, want)
	}
	if captured.mail.EnvelopeFrom != "updates@example.test" {
		t.Fatalf("confirmation envelope-from = %q", captured.mail.EnvelopeFrom)
	}
	marker := "/m/confirm/"
	start := strings.Index(captured.mail.BodyText, marker)
	if start < 0 {
		t.Fatalf("confirmation mail has no link: %q", captured.mail.BodyText)
	}
	rawConfirmation := strings.Fields(captured.mail.BodyText[start+len(marker):])[0]
	if strings.Contains(hex.EncodeToString(storedHash), rawConfirmation) {
		t.Fatal("database stored the raw confirmation token")
	}

	confirmed := request(http.MethodPost, "/m/confirm/"+rawConfirmation, nil, nil)
	replayed := request(http.MethodPost, "/m/confirm/"+rawConfirmation, nil, nil)
	invalidConfirm := request(http.MethodPost, "/m/confirm/not-a-token", nil, nil)
	if confirmed.Code != http.StatusOK || !strings.Contains(confirmed.Body.String(), "All set") || confirmed.Body.String() != replayed.Body.String() || replayed.Body.String() != invalidConfirm.Body.String() {
		t.Fatalf("confirmation oracle differs: first=(%d,%q), replay=(%d,%q), invalid=(%d,%q)", confirmed.Code, confirmed.Body.String(), replayed.Code, replayed.Body.String(), invalidConfirm.Code, invalidConfirm.Body.String())
	}
	var hashCleared bool
	if err := tdb.Super.QueryRow(ctx, `SELECT status::text, confirm_token_hash IS NULL FROM list_subscriber WHERE id=$1`, subscriberID).Scan(&status, &hashCleared); err != nil {
		t.Fatal(err)
	}
	if status != "active" || !hashCleared {
		t.Fatalf("confirmed subscriber status/hash-cleared = %q/%t", status, hashCleared)
	}
	var lifecycleEvents int
	if err := tdb.Super.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE payload->>'subscriber_id'=$1 AND topic IN ('mailing.subscriber.activated','mailing.subscriber.status_changed')`, subscriberID.String()).Scan(&lifecycleEvents); err != nil || lifecycleEvents != 2 {
		t.Fatalf("confirmation lifecycle events = %d, err=%v", lifecycleEvents, err)
	}

	unsubToken := codec.EncodeUnsubscribe(subscriberID, uuid.Nil)
	unsubscribed := request(http.MethodPost, "/m/u/"+unsubToken, []byte("List-Unsubscribe=One-Click"), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	validUnknown := request(http.MethodPost, "/m/u/"+codec.EncodeUnsubscribe(uuid.New(), uuid.Nil), nil, nil)
	bad := request(http.MethodPost, "/m/u/not-a-token", nil, nil)
	if unsubscribed.Code != http.StatusOK || unsubscribed.Body.Len() != 0 || validUnknown.Code != http.StatusOK || validUnknown.Body.Len() != 0 || bad.Code != http.StatusOK || bad.Body.Len() != 0 {
		t.Fatalf("unsubscribe responses differ: real=%d unknown=(%d,%q) bad=(%d,%q)", unsubscribed.Code, validUnknown.Code, validUnknown.Body.String(), bad.Code, bad.Body.String())
	}
	var suppressionCount int
	if err := tdb.Super.QueryRow(ctx, `SELECT s.status::text, count(ms.id) FROM list_subscriber s LEFT JOIN mailing_suppression ms ON ms.business_id=s.business_id AND ms.email=s.email WHERE s.id=$1 GROUP BY s.status`, subscriberID).Scan(&status, &suppressionCount); err != nil {
		t.Fatal(err)
	}
	if status != "unsubscribed" || suppressionCount != 1 {
		t.Fatalf("unsubscribe status/suppressions = %q/%d", status, suppressionCount)
	}

	s2sBody, _ := json.Marshal(map[string]any{"email": "api@example.test", "skip_confirmation": true})
	path := "/api/v1/mailing/s2s/" + key.PublishableKey + "/subscribers"
	ts := strconv.FormatInt(svc.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(key.Secret))
	_, _ = mac.Write(append([]byte(ts+"."+http.MethodPost+"."+path+"."), s2sBody...))
	s2sHeaders := map[string]string{"X-Mailing-Timestamp": ts, "X-Mailing-Signature": hex.EncodeToString(mac.Sum(nil)), "Content-Type": "application/json"}
	s2s := request(http.MethodPost, path, s2sBody, s2sHeaders)
	if s2s.Code != http.StatusCreated {
		t.Fatalf("S2S subscribe status/body = %d/%s", s2s.Code, s2s.Body.String())
	}
	var source string
	var attestor uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `SELECT consent_source::text, consent_attested_by FROM list_subscriber WHERE list_id=$1 AND email='api@example.test'`, list.ID).Scan(&source, &attestor); err != nil {
		t.Fatal(err)
	}
	if source != "api" || attestor != key.ID {
		t.Fatalf("S2S consent source/attestor = %q/%s", source, attestor)
	}
	deletePath := "/api/v1/mailing/s2s/" + key.PublishableKey + "/subscribers/api%40example.test"
	deleteMAC := hmac.New(sha256.New, []byte(key.Secret))
	_, _ = deleteMAC.Write([]byte(ts + "." + http.MethodDelete + "." + deletePath + "."))
	deleted := request(http.MethodDelete, deletePath, nil, map[string]string{
		"X-Mailing-Timestamp": ts, "X-Mailing-Signature": hex.EncodeToString(deleteMAC.Sum(nil)),
	})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("S2S unsubscribe status/body = %d/%s", deleted.Code, deleted.Body.String())
	}
	if err := tdb.Super.QueryRow(ctx, `SELECT status::text FROM list_subscriber WHERE list_id=$1 AND email='api@example.test'`, list.ID).Scan(&status); err != nil || status != "unsubscribed" {
		t.Fatalf("S2S unsubscribe status = %q, err=%v", status, err)
	}
	badHeaders := map[string]string{"X-Mailing-Timestamp": ts, "X-Mailing-Signature": strings.Repeat("00", sha256.Size), "Content-Type": "application/json"}
	badKnown := request(http.MethodPost, path, s2sBody, badHeaders)
	badUnknown := request(http.MethodPost, "/api/v1/mailing/s2s/mlk_unknown/subscribers", s2sBody, badHeaders)
	if badKnown.Code != http.StatusUnauthorized || badKnown.Body.String() != "{\"error\":\"unauthorized\"}\n" {
		t.Fatalf("unexpected S2S auth failure: status=%d body=%q", badKnown.Code, badKnown.Body.String())
	}
	if badKnown.Code != badUnknown.Code || badKnown.Body.String() != badUnknown.Body.String() {
		t.Fatalf("S2S auth oracle differs: bad known=(%d,%q), unknown=(%d,%q)", badKnown.Code, badKnown.Body.String(), badUnknown.Code, badUnknown.Body.String())
	}

	perKey.deny = true
	rateLimitedPublic := request(http.MethodPost, "/api/v1/mailing/public/"+key.PublishableKey+"/subscribe", subscribeBody, map[string]string{"Content-Type": "application/json"})
	if rateLimitedPublic.Code != http.StatusTooManyRequests {
		t.Fatalf("public per-key limit status/body = %d/%q", rateLimitedPublic.Code, rateLimitedPublic.Body.String())
	}
	rateLimitedS2S := request(http.MethodPost, path, s2sBody, s2sHeaders)
	if rateLimitedS2S.Code != http.StatusTooManyRequests {
		t.Fatalf("S2S per-key limit status/body = %d/%q", rateLimitedS2S.Code, rateLimitedS2S.Body.String())
	}
}
