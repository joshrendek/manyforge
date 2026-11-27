//go:build integration

package mailing_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	mailtoken "github.com/manyforge/manyforge/internal/mailing/token"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// seedPendingSubscriber inserts a pending subscriber with a confirmation token and
// returns the raw token.
func seedPendingSubscriber(ctx context.Context, t *testing.T, tdb *testdb.TestDB, codec *mailtoken.Codec, seed mailingSeed, listID uuid.UUID, email string) string {
	t.Helper()
	raw, hash, err := codec.NewConfirmation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, `INSERT INTO list_subscriber
		(business_id, tenant_root_id, list_id, email, status, consent_source, confirm_token_hash, confirm_expires_at)
		VALUES ($1,$1,$2,$3,'pending','public_form',$4, now() + interval '1 hour')`,
		seed.businessID, listID, email, hash); err != nil {
		t.Fatal(err)
	}
	return raw
}

func subscriberStatus(ctx context.Context, t *testing.T, tdb *testdb.TestDB, listID uuid.UUID, email string) (string, bool) {
	t.Helper()
	var status string
	err := tdb.Super.QueryRow(ctx, `SELECT status::text FROM list_subscriber WHERE list_id=$1 AND email=$2`, listID, email).Scan(&status)
	if err != nil {
		return "", false
	}
	return status, true
}

// TestPublicKeyBoundToPortalHostBusiness: on a custom hostname a list key or
// confirmation token of another business behaves exactly like an unknown one.
func TestPublicKeyBoundToPortalHostBusiness(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	a := seedMailingTenant(ctx, t, tdb)
	b := seedMailingTenant(ctx, t, tdb)
	master := bytes.Repeat([]byte{0x62}, 32)
	sealer, err := crypto.NewSealer(master)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := mailtoken.New(master)
	if err != nil {
		t.Fatal(err)
	}
	svc := &mailing.Service{
		DB: tdb.App, Sealer: sealer, Tokens: codec,
		Now: func() time.Time { return time.Now() },
	}
	list, err := svc.CreateList(ctx, a.principalID, a.businessID, mailing.ListInput{Name: "Updates", DoubleOptIn: false})
	if err != nil {
		t.Fatal(err)
	}
	key, err := svc.CreateListKey(ctx, a.principalID, a.businessID, list.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutBrand(ctx, a.principalID, a.businessID, mailing.BrandInput{Name: "Acme Co"}); err != nil {
		t.Fatal(err)
	}

	h := mailing.NewPublicHandler(svc, nil, sealer, func(*http.Request) string { return "203.0.113.7" })
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if v := req.Header.Get("X-Test-Portal"); v != "" {
				req = req.WithContext(httpx.WithPortalBusiness(req.Context(), uuid.MustParse(v)))
			}
			next.ServeHTTP(w, req)
		})
	})
	router.Route("/api/v1", h.PublicRoutes)
	h.RootRoutes(router)

	do := func(method, target, portal, body string) (int, string) {
		req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		if portal != "" {
			req.Header.Set("X-Test-Portal", portal)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	pa, pb := a.businessID.String(), b.businessID.String()

	t.Run("subscribe", func(t *testing.T) {
		path := "/api/v1/mailing/public/" + key.PublishableKey + "/subscribe"
		unknown := "/api/v1/mailing/public/mlk_unknown/subscribe"
		wantCode, wantBody := do(http.MethodPost, unknown, pb, `{"email":"foreign@example.com"}`)
		code, body := do(http.MethodPost, path, pb, `{"email":"foreign@example.com"}`)
		if code != wantCode || body != wantBody {
			t.Fatalf("foreign key = %d %q, unknown = %d %q", code, body, wantCode, wantBody)
		}
		if _, ok := subscriberStatus(ctx, t, tdb, list.ID, "foreign@example.com"); ok {
			t.Fatal("foreign-business key created a subscriber")
		}
		if code, body := do(http.MethodPost, path, pa, `{"email":"own@example.com"}`); code != wantCode || body != wantBody {
			t.Fatalf("own key = %d %q, want %d %q", code, body, wantCode, wantBody)
		}
		if _, ok := subscriberStatus(ctx, t, tdb, list.ID, "own@example.com"); !ok {
			t.Fatal("own-business key did not subscribe")
		}
		do(http.MethodPost, path, "", `{"email":"none@example.com"}`)
		if _, ok := subscriberStatus(ctx, t, tdb, list.ID, "none@example.com"); !ok {
			t.Fatal("no portal context did not subscribe")
		}
	})

	t.Run("brand", func(t *testing.T) {
		path := "/api/v1/mailing/public/" + key.PublishableKey + "/brand"
		wantCode, wantBody := do(http.MethodGet, "/api/v1/mailing/public/mlk_unknown/brand", pb, "")
		if code, body := do(http.MethodGet, path, pb, ""); code != wantCode || body != wantBody {
			t.Fatalf("foreign key = %d %q, unknown = %d %q", code, body, wantCode, wantBody)
		}
		for _, portal := range []string{pa, ""} {
			code, body := do(http.MethodGet, path, portal, "")
			if code != http.StatusOK || body == wantBody {
				t.Fatalf("portal %q brand = %d %q", portal, code, body)
			}
		}
	})

	t.Run("confirm", func(t *testing.T) {
		confirmList, err := svc.CreateList(ctx, a.principalID, a.businessID, mailing.ListInput{Name: "Confirm", DoubleOptIn: true})
		if err != nil {
			t.Fatal(err)
		}
		raw := seedPendingSubscriber(ctx, t, tdb, codec, a, confirmList.ID, "pending@example.com")
		unknownCode, unknownBody := do(http.MethodPost, "/m/confirm/not-a-token", pb, "")

		// Foreign business: identical response, subscriber stays pending, token still usable.
		code, body := do(http.MethodPost, "/m/confirm/"+raw, pb, "")
		if code != unknownCode || body != unknownBody {
			t.Fatalf("foreign confirm = %d %q, unknown = %d %q", code, body, unknownCode, unknownBody)
		}
		if st, _ := subscriberStatus(ctx, t, tdb, confirmList.ID, "pending@example.com"); st != "pending" {
			t.Fatalf("after foreign confirm status = %q, want pending", st)
		}
		var hashKept bool
		if err := tdb.Super.QueryRow(ctx, `SELECT confirm_token_hash IS NOT NULL FROM list_subscriber WHERE list_id=$1 AND email=$2`, confirmList.ID, "pending@example.com").Scan(&hashKept); err != nil || !hashKept {
			t.Fatalf("token consumed by foreign confirm: kept=%v err=%v", hashKept, err)
		}

		// Own business confirms.
		if code, body := do(http.MethodPost, "/m/confirm/"+raw, pa, ""); code != unknownCode || body != unknownBody {
			t.Fatalf("own confirm = %d %q, want %d %q", code, body, unknownCode, unknownBody)
		}
		if st, _ := subscriberStatus(ctx, t, tdb, confirmList.ID, "pending@example.com"); st != "active" {
			t.Fatalf("after own confirm status = %q, want active", st)
		}

		// No portal context: unchanged behaviour.
		raw2 := seedPendingSubscriber(ctx, t, tdb, codec, a, confirmList.ID, "second@example.com")
		do(http.MethodPost, "/m/confirm/"+raw2, "", "")
		if st, _ := subscriberStatus(ctx, t, tdb, confirmList.ID, "second@example.com"); st != "active" {
			t.Fatalf("instance-host confirm status = %q, want active", st)
		}

		// A well-formed token minted by the codec but never stored behaves exactly
		// like a malformed one on a portal hostname, and changes no subscriber.
		seedPendingSubscriber(ctx, t, tdb, codec, a, confirmList.ID, "third@example.com")
		snapshot := func() string {
			t.Helper()
			var out string
			if err := tdb.Super.QueryRow(ctx, `SELECT coalesce(string_agg(
				email || ':' || status::text || ':' || coalesce(encode(confirm_token_hash, 'hex'), '-') || ':' || coalesce(confirmed_at::text, '-') || ':' || updated_at::text,
				',' ORDER BY email), '') FROM list_subscriber`).Scan(&out); err != nil {
				t.Fatal(err)
			}
			return out
		}
		minted, _, err := codec.NewConfirmation()
		if err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		for _, portal := range []string{pa, pb} {
			if code, body := do(http.MethodPost, "/m/confirm/"+minted, portal, ""); code != unknownCode || body != unknownBody {
				t.Fatalf("portal %s unstored well-formed token = %d %q, unknown = %d %q", portal, code, body, unknownCode, unknownBody)
			}
		}
		if after := snapshot(); after != before {
			t.Fatalf("unstored token changed subscribers:\nbefore %s\nafter  %s", before, after)
		}
	})
}
