package githubapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConvertManifestParsesCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app-manifests/thecode/conversions" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 99, "slug": "mf-review", "client_id": "Iv1.x",
			"client_secret": "cs", "pem": "-----BEGIN RSA PRIVATE KEY-----k", "webhook_secret": "whsec"})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), APIBase: srv.URL, WebBase: srv.URL}
	creds, err := c.ConvertManifest(context.Background(), "thecode")
	if err != nil {
		t.Fatalf("ConvertManifest: %v", err)
	}
	if creds.AppID != 99 || creds.Slug != "mf-review" || creds.PrivateKeyPEM == "" || creds.WebhookSecret != "whsec" {
		t.Fatalf("got %+v", creds)
	}
}

func TestListUserInstallationsExtractsIDsAndAccounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer utoken" {
			t.Errorf("auth = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1,
			"installations": []map[string]any{{"id": 22, "account": map[string]any{"login": "bluescripts-net", "type": "Organization"}}}})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), APIBase: srv.URL, WebBase: srv.URL}
	got, err := c.ListUserInstallations(context.Background(), "utoken")
	if err != nil {
		t.Fatalf("ListUserInstallations: %v", err)
	}
	if len(got) != 1 || got[0].ID != 22 || got[0].Login != "bluescripts-net" || got[0].Type != "Organization" {
		t.Fatalf("got %+v", got)
	}
}
