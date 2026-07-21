package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCodexBackendModels_ListModels_visibleOrderedCached(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/models" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("client_version") == "" {
			t.Error("missing client_version")
		}
		if r.Header.Get("Authorization") != "Bearer tok-abc" {
			t.Errorf("authz = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("ChatGPT-Account-Id") != "acct-9" {
			t.Errorf("account header = %q", r.Header.Get("ChatGPT-Account-Id"))
		}
		// Mixed visibility + out-of-order priority: only visibility=="list" is returned, priority-ordered.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"slug": "gpt-5.4", "visibility": "hide", "priority": 16},
				{"slug": "gpt-5.6-terra", "visibility": "list", "priority": 2},
				{"slug": "gpt-5.6-sol", "visibility": "list", "priority": 1},
				{"slug": "codex-auto-review", "visibility": "hide", "priority": 43},
				{"slug": "gpt-5.5", "visibility": "list", "priority": 7},
			},
		})
	}))
	defer srv.Close()

	c := &CodexBackendModels{HTTP: srv.Client(), Base: srv.URL, now: time.Now}
	got, err := c.ListModels(context.Background(), "tok-abc", "acct-9")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.5"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q want %q (full %v)", i, got[i], want[i], got)
		}
	}
	// second call is served from the per-account cache (no extra upstream request)
	if _, err := c.ListModels(context.Background(), "tok-abc", "acct-9"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 upstream call (cached), got %d", calls)
	}
}

func TestCodexBackendModels_ListModels_upstreamErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &CodexBackendModels{HTTP: srv.Client(), Base: srv.URL, now: time.Now}
	if _, err := c.ListModels(context.Background(), "t", "a"); err == nil {
		t.Fatal("expected an error on upstream 401")
	}
}
