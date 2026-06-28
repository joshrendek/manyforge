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

func TestPostReview_IdempotentReuse(t *testing.T) {
	var posted bool
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // listing existing reviews — one carries the marker
			_, _ = w.Write([]byte(`[{"id":55,"body":"prior\n\n<!-- manyforge-review-id: cr-1 -->","html_url":"http://x/55"}]`))
			return
		}
		posted = true
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":99,"html_url":"http://x/99"}`))
	})
	ref, err := c.PostReview(t.Context(), 42, connectors.Review{Body: "new", DedupKey: "cr-1"})
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if posted {
		t.Fatal("must NOT post a duplicate when a review with the marker already exists")
	}
	if ref.ExternalID != "55" {
		t.Fatalf("should reuse existing review 55, got %s", ref.ExternalID)
	}
}

func TestPostReview_PostsAndEmbedsMarkerWhenNoMatch(t *testing.T) {
	var gotBody string
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // no existing reviews → must post
			_, _ = w.Write([]byte(`[]`))
			return
		}
		var b struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotBody = b.Body
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":77,"html_url":"http://x/77"}`))
	})
	ref, err := c.PostReview(t.Context(), 42, connectors.Review{Body: "fresh", DedupKey: "cr-2"})
	if err != nil || ref.ExternalID != "77" {
		t.Fatalf("got %+v err %v", ref, err)
	}
	if !strings.Contains(gotBody, "<!-- manyforge-review-id: cr-2 -->") {
		t.Fatalf("posted body must embed the dedup marker, got %q", gotBody)
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

func TestChangedFiles(t *testing.T) {
	c := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/pulls/42/files") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
			{"filename":"a.go","patch":"@@ -1,1 +1,2 @@\n ctx\n+added\n"},
			{"filename":"bin.png","patch":""}
		]`))
	})
	got, err := c.ChangedFiles(t.Context(), 42)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 files, got %d: %+v", len(got), got)
	}
	var a connectors.ChangedFile
	for _, f := range got {
		if f.Path == "a.go" {
			a = f
		}
	}
	if a.Patch == "" {
		t.Fatalf("a.go patch must be retained, got empty")
	}
	if !a.Commentable[1] || !a.Commentable[2] {
		t.Fatalf("a.go lines 1,2 expected commentable; got %v", a.Commentable)
	}
}
