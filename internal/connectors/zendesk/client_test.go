package zendesk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestClient builds a *client pointed at srvURL with the given credentials.
func newTestClient(srvURL, email, apiToken, webhookSecret string, httpClient *http.Client) *client {
	return &client{
		httpClient:    httpClient,
		baseURL:       srvURL,
		email:         email,
		apiToken:      apiToken,
		webhookSecret: webhookSecret,
	}
}

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestFetchIssue(t *testing.T) {
	var gotAuthUser, gotAuthPass string
	var sawInclude string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthUser, gotAuthPass, _ = r.BasicAuth()
		switch {
		case r.URL.Path == "/api/v2/tickets/12345.json":
			sawInclude = r.URL.Query().Get("include")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(mustFixture(t, "ticket.json"))
		case r.URL.Path == "/api/v2/tickets/12345/comments.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(mustFixture(t, "comments.json"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "ops@acme.test", "tok-123", "secret", srv.Client())
	issue, err := c.FetchIssue(context.Background(), "12345")
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}

	// Zendesk API-token Basic auth: username is "<email>/token", password is the token.
	if gotAuthUser != "ops@acme.test/token" || gotAuthPass != "tok-123" {
		t.Errorf("basic auth = %q:%q, want ops@acme.test/token:tok-123", gotAuthUser, gotAuthPass)
	}
	if sawInclude != "users" {
		t.Errorf("include = %q, want users (requester sideload)", sawInclude)
	}
	if issue.ExternalID != "12345" {
		t.Errorf("ExternalID = %q, want 12345", issue.ExternalID)
	}
	if issue.Title != "Login button is broken" {
		t.Errorf("Title = %q", issue.Title)
	}
	if issue.Status != "open" || issue.Priority != "high" {
		t.Errorf("status/priority = %q/%q", issue.Status, issue.Priority)
	}
	if issue.ReporterEmail != "reporter@acme.test" || issue.ReporterName != "Reporter Person" {
		t.Errorf("reporter = %q/%q (sideload match failed)", issue.ReporterEmail, issue.ReporterName)
	}
	if !strings.HasSuffix(issue.URL, "/agent/tickets/12345") {
		t.Errorf("URL = %q, want agent-facing /agent/tickets/12345", issue.URL)
	}
	if len(issue.Comments) != 2 || issue.Comments[0].ExternalID != "5001" || issue.Comments[0].Body != "First comment" {
		t.Errorf("comments = %+v", issue.Comments)
	}
}

func TestFetchIssue_RejectsTraversalID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be reached for an invalid ticket id")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "ops@acme.test", "tok-123", "secret", srv.Client())
	if _, err := c.FetchIssue(context.Background(), "../../admin"); !errors.Is(err, ErrBadTicketID) {
		t.Fatalf("err = %v, want Is(ErrBadTicketID)", err)
	}
}

func TestFetchIssue_DoesNotLeakAPIToken(t *testing.T) {
	apiToken := "super-secret-token-xyz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom ` + apiToken + `"}`))
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "ops@acme.test", apiToken, "secret", srv.Client())
	_, err := c.FetchIssue(context.Background(), "12345")
	if err == nil {
		t.Fatal("want error on 500")
	}
	if strings.Contains(err.Error(), apiToken) {
		t.Fatalf("error leaks token/upstream body: %v", err)
	}
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want Is(ErrUpstream)", err)
	}
}
