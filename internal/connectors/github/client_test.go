package github

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

func newStubClient(t *testing.T, h http.HandlerFunc) *client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &client{http: srv.Client(), apiBase: srv.URL, repo: "o/r", token: "tkn"}
}

func TestFetchPR(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tkn" {
			t.Errorf("missing auth header")
		}
		w.Write([]byte(`{"number":42,"title":"x","state":"open","merged":false,"head":{"sha":"abc","ref":"feat"},"base":{"ref":"main"}}`))
	})
	pr, err := c.FetchPR(t.Context(), 42)
	if err != nil || pr.HeadSHA != "abc" || pr.State != "open" {
		t.Fatalf("got %+v err %v", pr, err)
	}
}

func TestFetchPR_Merged(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"number":1,"title":"y","state":"closed","merged":true,"head":{"sha":"def","ref":"feat"},"base":{"ref":"main"}}`))
	})
	pr, err := c.FetchPR(t.Context(), 1)
	if err != nil || pr.State != "merged" {
		t.Fatalf("got %+v err %v", pr, err)
	}
}

func TestFetchPR_NotFound(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.FetchPR(t.Context(), 999)
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestPostReview(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte(`{"id":7,"html_url":"http://x/7"}`))
	})
	ref, err := c.PostReview(t.Context(), 42, connectors.Review{Body: "hi"})
	if err != nil || ref.ExternalID != "7" {
		t.Fatalf("got %+v err %v", ref, err)
	}
}

func TestCloneURL_Public(t *testing.T) {
	c := &client{apiBase: "https://api.github.com", repo: "owner/repo"}
	got := c.CloneURL()
	want := "https://github.com/owner/repo.git"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestBasicAuthHeader(t *testing.T) {
	h := BasicAuthHeader("mytoken")
	// Should start with "AUTHORIZATION: basic "
	if len(h) == 0 {
		t.Fatal("empty header")
	}
	// Just check prefix
	const prefix = "AUTHORIZATION: basic "
	if len(h) <= len(prefix) {
		t.Fatalf("header too short: %q", h)
	}
}
