package github

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/errs"
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
		_, _ = w.Write([]byte(`{"number":42,"title":"x","state":"open","merged":false,"head":{"sha":"abc","ref":"feat"},"base":{"ref":"main"}}`))
	})
	pr, err := c.FetchPR(t.Context(), 42)
	if err != nil || pr.HeadSHA != "abc" || pr.State != "open" {
		t.Fatalf("got %+v err %v", pr, err)
	}
}

func TestFetchPR_Merged(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"number":1,"title":"y","state":"closed","merged":true,"head":{"sha":"def","ref":"feat"},"base":{"ref":"main"}}`))
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
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("expected errs.ErrNotFound, got %v", err)
	}
}

func TestPostReview(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tkn" {
			t.Errorf("missing or incorrect auth header")
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":7,"html_url":"http://x/7"}`))
	})
	ref, err := c.PostReview(t.Context(), 42, connectors.Review{Body: "hi"})
	if err != nil || ref.ExternalID != "7" {
		t.Fatalf("got %+v err %v", ref, err)
	}
}

func TestChangedLines(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/pulls/42/files") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		// One page, two files (fewer than 100 → no second page fetched).
		_, _ = w.Write([]byte(`[
			{"filename":"a.go","patch":"@@ -1,1 +1,2 @@\n ctx\n+added\n"},
			{"filename":"bin.png","patch":""}
		]`))
	})
	got, err := c.ChangedLines(t.Context(), 42)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if !got["a.go"][1] || !got["a.go"][2] {
		t.Fatalf("a.go lines 1,2 expected commentable; got %v", got["a.go"])
	}
	if _, ok := got["bin.png"]; !ok || len(got["bin.png"]) != 0 {
		t.Fatalf("bin.png should appear with no commentable lines; got %v", got["bin.png"])
	}
}

func TestPostReviewWithInlineComments(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Body     string `json:"body"`
			CommitID string `json:"commit_id"`
			Comments []struct {
				Path string `json:"path"`
				Line int    `json:"line"`
				Side string `json:"side"`
				Body string `json:"body"`
			} `json:"comments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if body.CommitID != "headsha" {
			t.Errorf("commit_id=%q", body.CommitID)
		}
		if len(body.Comments) != 1 || body.Comments[0].Path != "a.go" ||
			body.Comments[0].Line != 11 || body.Comments[0].Side != "RIGHT" {
			t.Errorf("comments=%+v", body.Comments)
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":9,"html_url":"http://x/9"}`))
	})
	ref, err := c.PostReview(t.Context(), 42, connectors.Review{
		Body:     "summary",
		CommitID: "headsha",
		Comments: []connectors.ReviewComment{{Path: "a.go", Line: 11, Body: "inline"}},
	})
	if err != nil || ref.ExternalID != "9" {
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
	token := "mytoken"
	h := BasicAuthHeader(token)
	// Should start with "AUTHORIZATION: basic "
	if len(h) == 0 {
		t.Fatal("empty header")
	}
	const prefix = "AUTHORIZATION: basic "
	if !strings.HasPrefix(h, prefix) {
		t.Fatalf("header missing prefix: %q", h)
	}
	if len(h) <= len(prefix) {
		t.Fatalf("header too short: %q", h)
	}

	// Decode and verify the base64 payload
	base64Part := h[len(prefix):]
	decoded, err := base64.StdEncoding.DecodeString(base64Part)
	if err != nil {
		t.Fatalf("failed to decode base64: %v", err)
	}
	expected := "x-access-token:" + token
	if string(decoded) != expected {
		t.Fatalf("payload mismatch: got %q, want %q", string(decoded), expected)
	}
}
