package mailing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

func TestSetupSeparatesRelayAndAPIPrerequisites(t *testing.T) {
	cfg := SetupConfig{MailingKeyConfigured: true, PublicBaseURL: "https://private-origin.example.test"}
	request := func() (SetupStatus, string) {
		t.Helper()
		router := chi.NewRouter()
		NewSetupHandler(cfg).ReadRoutes(router)
		req := httptest.NewRequest(http.MethodGet, "/businesses/"+uuid.NewString()+"/mailing/setup", nil)
		req = req.WithContext(httpx.WithPrincipal(req.Context(), uuid.New()))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("setup status=%d body=%s", response.Code, response.Body)
		}
		var status SetupStatus
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		return status, response.Body.String()
	}
	blockers := func(status SetupStatus, mode string) []string {
		var out []string
		for _, check := range status.Checks {
			if check.Status == "blocked" && slices.Contains(check.RequiredFor, mode) {
				out = append(out, check.ID)
			}
		}
		return out
	}
	status, body := request()
	if strings.Contains(body, cfg.PublicBaseURL) {
		t.Fatal("setup exposes the configured origin instead of readiness")
	}
	for _, mode := range []string{"resend", "ses"} {
		if got := blockers(status, mode); len(got) != 0 {
			t.Fatalf("%s blocked by relay-only requirements: %v", mode, got)
		}
	}
	if got := blockers(status, "relay"); !slices.Equal(got, []string{"smtp_relay", "dkim_key"}) {
		t.Fatalf("relay blockers = %v", got)
	}
	cfg.OutboundMailDisabled = true
	disabled, _ := request()
	if got := blockers(disabled, "resend"); !slices.Equal(got, []string{"outbound_enabled"}) {
		t.Fatalf("configured provider bypasses instance disable switch: %v", got)
	}
}
