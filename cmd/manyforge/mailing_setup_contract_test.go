//go:build contract

package main

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// A missing master key must remain diagnosable without making deployment
// configuration public or bypassing the business's mailing.read permission.
func TestMailingSetupAvailableWithoutKeyAndPermissionGated(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "k1", priv, map[string]ed25519.PublicKey{"k1": pub})
	if err != nil {
		t.Fatal(err)
	}
	token, err := ring.Sign(uuid.New(), time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name          string
		authenticated bool
		allowed       bool
		wantStatus    int
	}{
		{"anonymous", false, true, http.StatusUnauthorized},
		{"no mailing permission", true, false, http.StatusNotFound},
		{"authorized with mailing disabled", true, true, http.StatusOK},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h := testHandlers()
			h.mailing, h.mailingPublic, h.mailingWebhook, h.automations = nil, nil, nil, nil
			h.mailingSetup = mailing.NewSetupHandler(mailing.SetupConfig{OutboundMailDisabled: true})
			h.mailingRead = func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !scenario.allowed {
						http.NotFound(w, r)
						return
					}
					next.ServeHTTP(w, r)
				})
			}
			router := httpx.NewRouter(ring)
			mountAPIRoutes(router, h)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/businesses/00000000-0000-4000-8000-000000000001/mailing/setup", nil)
			if scenario.authenticated {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != scenario.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, scenario.wantStatus, response.Body)
			}
			if response.Code != http.StatusOK {
				return
			}
			var result mailing.SetupStatus
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			blocked := map[string]bool{}
			for _, check := range result.Checks {
				if check.Status == "blocked" && check.Action != "" {
					blocked[check.ID] = true
				}
			}
			if !blocked["mailing_key"] || !blocked["outbound_enabled"] {
				t.Fatalf("disabled prerequisites lack actionable blockers: %+v", result)
			}
		})
	}
}
