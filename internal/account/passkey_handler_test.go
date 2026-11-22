package account

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

func TestPasskeyManagementRequiresFreshLoginAndExactOrigin(t *testing.T) {
	t.Parallel()
	const origin = "https://hub.example.test"
	const path = "/api/passkeys/register/begin"
	ring, err := auth.NewDevKeyRing("test", "test")
	if err != nil {
		t.Fatal(err)
	}
	passkeys, err := NewPasskeys(origin)
	if err != nil {
		t.Fatal(err)
	}
	router := httpx.NewRouter(ring)
	NewHandler(&Service{Passkeys: passkeys}).BrowserPasskeyRoutes(router, func(next http.Handler) http.Handler {
		return next
	})
	principalID := uuid.New()
	now := time.Now()
	fresh, err := ring.SignForLogin(principalID, uuid.New(), auth.LoginPassword, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := ring.SignForLogin(principalID, uuid.New(), auth.LoginPassword, time.Hour, now.Add(-11*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := ring.Sign(principalID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		token  string
		origin []string
		want   int
	}{
		{"anonymous", "", []string{origin}, http.StatusUnauthorized},
		{"refreshed session", refreshed, []string{origin}, http.StatusForbidden},
		{"stale login", stale, []string{origin}, http.StatusForbidden},
		{"missing origin", fresh, nil, http.StatusForbidden},
		{"sibling origin", fresh, []string{"https://other.example.test"}, http.StatusForbidden},
		{"duplicate origin", fresh, []string{origin, origin}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header["Origin"] = tc.origin
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if recorder.Code != tc.want {
				t.Fatalf("status %d, want %d; body %s", recorder.Code, tc.want, recorder.Body)
			}
		})
	}
}
