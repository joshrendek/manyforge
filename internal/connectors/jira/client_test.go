package jira

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/connectors"
)

// loadGolden reads a golden fixture from testdata/.
func loadGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loadGolden %s: %v", name, err)
	}
	return b
}

// newTestClient builds a *client pointed at srvURL with the provided credentials.
func newTestClient(srvURL, email, apiToken, webhookSecret string, httpClient *http.Client) *client {
	return &client{
		httpClient:    httpClient,
		baseURL:       srvURL,
		email:         email,
		apiToken:      apiToken,
		webhookSecret: webhookSecret,
	}
}

// resolvedConnector is a test helper that builds a connectors.ResolvedConnector.
func resolvedConnector(baseURL, email, apiToken, webhookSecret string, allowPrivate bool) connectors.ResolvedConnector {
	return connectors.ResolvedConnector{
		BaseURL:             baseURL,
		AllowPrivateBaseURL: allowPrivate,
		Credential: connectors.Credential{
			Email:         email,
			APIToken:      apiToken,
			WebhookSecret: webhookSecret,
		},
	}
}

// ── FetchIssue ───────────────────────────────────────────────────────────────

func TestFetchIssue(t *testing.T) {
	issueBody := loadGolden(t, "issue.json")
	commentsBody := loadGolden(t, "comments.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify Basic auth header on every request.
		email, token, ok := r.BasicAuth()
		if !ok || email != "user@example.com" || token != "test-api-token" {
			t.Errorf("FetchIssue: bad basic auth: ok=%v email=%q token=%q", ok, email, token)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/comment"):
			// /rest/api/3/issue/PROJ-42/comment
			if r.URL.Path != "/rest/api/3/issue/PROJ-42/comment" {
				t.Errorf("FetchIssue comments: unexpected path %q", r.URL.Path)
			}
			_, _ = w.Write(commentsBody)
		default:
			// /rest/api/3/issue/PROJ-42
			if r.URL.Path != "/rest/api/3/issue/PROJ-42" {
				t.Errorf("FetchIssue issue: unexpected path %q", r.URL.Path)
			}
			if q := r.URL.Query().Get("fields"); q != "summary,status,priority,reporter,updated" {
				t.Errorf("FetchIssue: fields query = %q", q)
			}
			_, _ = w.Write(issueBody)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "user@example.com", "test-api-token", "secret", srv.Client())
	issue, err := c.FetchIssue(context.Background(), "PROJ-42")
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}

	if issue.ExternalID != "PROJ-42" {
		t.Errorf("ExternalID = %q, want PROJ-42", issue.ExternalID)
	}
	if issue.Title != "Login button does not work on mobile" {
		t.Errorf("Title = %q", issue.Title)
	}
	if issue.Status != "In Progress" {
		t.Errorf("Status = %q, want In Progress", issue.Status)
	}
	if issue.Priority != "High" {
		t.Errorf("Priority = %q, want High", issue.Priority)
	}
	if issue.ReporterEmail != "alice@example.com" {
		t.Errorf("ReporterEmail = %q, want alice@example.com", issue.ReporterEmail)
	}
	if issue.ReporterName != "Alice Smith" {
		t.Errorf("ReporterName = %q, want Alice Smith", issue.ReporterName)
	}
	if issue.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
	if !strings.HasSuffix(issue.URL, "/browse/PROJ-42") {
		t.Errorf("URL = %q, want suffix /browse/PROJ-42", issue.URL)
	}

	// Comments.
	if len(issue.Comments) != 2 {
		t.Fatalf("len(Comments) = %d, want 2", len(issue.Comments))
	}
	c1 := issue.Comments[0]
	if c1.ExternalID != "20001" {
		t.Errorf("Comments[0].ExternalID = %q, want 20001", c1.ExternalID)
	}
	if c1.Author != "Alice Smith" {
		t.Errorf("Comments[0].Author = %q, want Alice Smith", c1.Author)
	}
	if !strings.Contains(c1.Body, "iOS and Android") {
		t.Errorf("Comments[0].Body = %q, want to contain 'iOS and Android'", c1.Body)
	}
	if c1.CreatedAt.IsZero() {
		t.Error("Comments[0].CreatedAt is zero")
	}
}

// ── ListUpdatedSince ─────────────────────────────────────────────────────────

func TestListUpdatedSince(t *testing.T) {
	searchBody := loadGolden(t, "search_updated.json")
	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email, token, ok := r.BasicAuth()
		if !ok || email != "user@example.com" || token != "test-api-token" {
			t.Errorf("ListUpdatedSince: bad basic auth")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/rest/api/3/search/jql" {
			t.Errorf("ListUpdatedSince: path = %q, want /rest/api/3/search/jql", r.URL.Path)
		}
		jql := r.URL.Query().Get("jql")
		if !strings.Contains(jql, "updated >= ") {
			t.Errorf("ListUpdatedSince: jql = %q, missing 'updated >= '", jql)
		}
		if !strings.Contains(jql, "2026-06-01") {
			t.Errorf("ListUpdatedSince: jql = %q, missing '2026-06-01'", jql)
		}
		if r.URL.Query().Get("fields") != "updated" {
			t.Errorf("ListUpdatedSince: fields = %q, want 'updated'", r.URL.Query().Get("fields"))
		}
		if r.URL.Query().Get("nextPageToken") != "" {
			t.Errorf("ListUpdatedSince: first page must not send nextPageToken, got %q", r.URL.Query().Get("nextPageToken"))
		}
		if r.URL.Query().Get("maxResults") == "" {
			t.Errorf("ListUpdatedSince: maxResults missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(searchBody)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "user@example.com", "test-api-token", "secret", srv.Client())
	keys, err := c.ListUpdatedSince(context.Background(), since)
	if err != nil {
		t.Fatalf("ListUpdatedSince: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("len(keys) = %d, want 3", len(keys))
	}
	want := []string{"PROJ-42", "PROJ-43", "PROJ-44"}
	for i, k := range keys {
		if k != want[i] {
			t.Errorf("keys[%d] = %q, want %q", i, k, want[i])
		}
	}
}

// I3 pin: ListUpdatedSince must page through ALL results, not stop at Jira's
// default page size. Two pages, total=3 → all 3 keys returned in order.
func TestListUpdatedSince_Paginates(t *testing.T) {
	page1 := loadGolden(t, "search_updated_page1.json")
	page2 := loadGolden(t, "search_updated_page2.json")
	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/rest/api/3/search/jql" {
			t.Errorf("path = %q, want /rest/api/3/search/jql", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("nextPageToken") {
		case "":
			_, _ = w.Write(page1)
		case "PAGE2":
			_, _ = w.Write(page2)
		default:
			t.Errorf("unexpected nextPageToken = %q", r.URL.Query().Get("nextPageToken"))
			_, _ = w.Write([]byte(`{"issues":[]}`))
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "user@example.com", "test-api-token", "secret", srv.Client())
	keys, err := c.ListUpdatedSince(context.Background(), since)
	if err != nil {
		t.Fatalf("ListUpdatedSince: %v", err)
	}
	if requests != 2 {
		t.Errorf("server saw %d requests, want 2 (one per page)", requests)
	}
	want := []string{"PROJ-42", "PROJ-43", "PROJ-44"}
	if len(keys) != len(want) {
		t.Fatalf("len(keys) = %d, want %d: %v", len(keys), len(want), keys)
	}
	for i, k := range keys {
		if k != want[i] {
			t.Errorf("keys[%d] = %q, want %q", i, k, want[i])
		}
	}
}

// ── VerifyWebhook ────────────────────────────────────────────────────────────

func TestVerifyWebhook_Valid(t *testing.T) {
	const secret = "my-webhook-secret"
	body := loadGolden(t, "webhook_issue_updated.json")

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	c := &client{webhookSecret: secret}
	if err := c.VerifyWebhook(http.Header{"X-Hub-Signature": []string{sig}}, body); err != nil {
		t.Fatalf("VerifyWebhook valid: %v", err)
	}
}

func TestVerifyWebhook_Forged(t *testing.T) {
	const secret = "my-webhook-secret"
	body := loadGolden(t, "webhook_issue_updated.json")

	mac := hmac.New(sha256.New, []byte("wrong-secret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	c := &client{webhookSecret: secret}
	err := c.VerifyWebhook(http.Header{"X-Hub-Signature": []string{sig}}, body)
	if err == nil {
		t.Fatal("VerifyWebhook forged: expected error, got nil")
	}
	if !errors.Is(err, ErrBadSig) {
		t.Errorf("VerifyWebhook forged: err = %v, want Is(ErrBadSig)", err)
	}
}

func TestVerifyWebhook_MissingHeader(t *testing.T) {
	c := &client{webhookSecret: "secret"}
	err := c.VerifyWebhook(http.Header{}, []byte(`{"test":1}`))
	if err == nil {
		t.Fatal("VerifyWebhook missing header: expected error, got nil")
	}
	if !errors.Is(err, ErrBadSig) {
		t.Errorf("VerifyWebhook missing header: err = %v, want Is(ErrBadSig)", err)
	}
}

// C1 pin: a client with NO webhook secret must FAIL CLOSED — even a "correct"
// signature computed against the empty key (i.e. the forgeable case) is rejected.
func TestVerifyWebhook_EmptySecretRejected(t *testing.T) {
	body := []byte(`{"webhookEvent":"jira:issue_updated","issue":{"key":"PROJ-42"}}`)

	// Compute the signature an attacker WOULD produce against the empty key.
	mac := hmac.New(sha256.New, []byte(""))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	c := &client{webhookSecret: ""} // unconfigured secret
	err := c.VerifyWebhook(http.Header{"X-Hub-Signature": []string{sig}}, body)
	if err == nil {
		t.Fatal("VerifyWebhook empty secret: expected rejection, got nil (FORGEABLE)")
	}
	if !errors.Is(err, ErrBadSig) {
		t.Errorf("VerifyWebhook empty secret: err = %v, want Is(ErrBadSig)", err)
	}
}

// ── DecodeWebhook ────────────────────────────────────────────────────────────

func TestDecodeWebhook(t *testing.T) {
	body := loadGolden(t, "webhook_issue_updated.json")

	c := &client{}
	event, err := c.DecodeWebhook(body)
	if err != nil {
		t.Fatalf("DecodeWebhook: %v", err)
	}
	if event.ExternalID != "PROJ-42" {
		t.Errorf("ExternalID = %q, want PROJ-42", event.ExternalID)
	}
	if event.Kind != "issue.updated" {
		t.Errorf("Kind = %q, want issue.updated", event.Kind)
	}
	if event.DeliveryID == "" {
		t.Error("DeliveryID is empty")
	}
	if !strings.Contains(event.DeliveryID, "PROJ-42") {
		t.Errorf("DeliveryID = %q, want to contain PROJ-42", event.DeliveryID)
	}
}

// C2 pin: DecodeWebhook must reject a malformed/path-traversal issue key so it
// never enters the sync pipeline.
func TestDecodeWebhook_RejectsBadKey(t *testing.T) {
	for _, badKey := range []string{"PROJ-1/../../admin", "../secret", "proj-1", "PROJ-1 OR 1=1", ""} {
		body := []byte(fmt.Sprintf(`{"webhookEvent":"jira:issue_updated","timestamp":1717228800000,"issue":{"key":%q}}`, badKey))
		c := &client{}
		_, err := c.DecodeWebhook(body)
		if err == nil {
			t.Errorf("DecodeWebhook(%q): expected error, got nil", badKey)
			continue
		}
		if !errors.Is(err, ErrBadIssueKey) {
			t.Errorf("DecodeWebhook(%q): err = %v, want Is(ErrBadIssueKey)", badKey, err)
		}
	}
}

// ── Security pins ─────────────────────────────────────────────────────────────

// Pin: a non-2xx response from Jira MUST NOT expose the API token in the error string.
func TestFetchIssue_DoesNotLeakAPIToken(t *testing.T) {
	const apiToken = "SUPER_SECRET_API_TOKEN_xyz_do_not_leak"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// Simulate an upstream body that mentions the token — must NOT reach caller.
		_, _ = io.WriteString(w, fmt.Sprintf(`{"errorMessages":["Invalid token: %s"]}`, apiToken))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "user@example.com", apiToken, "secret", srv.Client())
	_, err := c.FetchIssue(context.Background(), "PROJ-42")
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	if strings.Contains(err.Error(), apiToken) {
		t.Errorf("error leaks api_token: %q", err.Error())
	}
}

// Pin: 500 upstream body MUST NOT appear in the returned error.
func TestListUpdatedSince_DoesNotLeakUpstreamBody(t *testing.T) {
	const upstreamSecret = "UPSTREAM_INTERNAL_db_constraint_name_do_not_leak"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"error":"%s"}`, upstreamSecret))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "user@example.com", "token", "secret", srv.Client())
	_, err := c.ListUpdatedSince(context.Background(), time.Now())
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
	if strings.Contains(err.Error(), upstreamSecret) {
		t.Errorf("error leaks upstream body: %q", err.Error())
	}
}

// C2 pin: a path-traversal issue key must be rejected BEFORE any URL build /
// HTTP call. The server handler t.Fatals if it is ever hit.
func TestFetchIssue_RejectsTraversalKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server must NOT be hit for a traversal key; got request to %q", r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "user@example.com", "token", "secret", srv.Client())
	for _, badKey := range []string{"PROJ-1/../../admin", "../../secret", "PROJ-1/comment", ""} {
		_, err := c.FetchIssue(context.Background(), badKey)
		if err == nil {
			t.Errorf("FetchIssue(%q): expected error, got nil", badKey)
			continue
		}
		if !errors.Is(err, ErrBadIssueKey) {
			t.Errorf("FetchIssue(%q): err = %v, want Is(ErrBadIssueKey)", badKey, err)
		}
	}

	// PostComment / TransitionStatus must reject too (no HTTP call).
	if _, err := c.PostComment(context.Background(), "PROJ-1/../admin", "hi"); !errors.Is(err, ErrBadIssueKey) {
		t.Errorf("PostComment traversal: err = %v, want Is(ErrBadIssueKey)", err)
	}
	if err := c.TransitionStatus(context.Background(), "PROJ-1/../admin", "Done"); !errors.Is(err, ErrBadIssueKey) {
		t.Errorf("TransitionStatus traversal: err = %v, want Is(ErrBadIssueKey)", err)
	}
}

// ── CreateIssue ───────────────────────────────────────────────────────────────

func TestCreateIssue(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadGolden(t, "create_issue_response.json"))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "ops@acme.test", "tok", "whsec", srv.Client())
	iss, err := c.CreateIssue(context.Background(), connectors.ExternalIssueDraft{
		ProjectKey: "SUP", IssueType: "Task", Summary: "Login broken", Description: "user can't log in",
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if iss.ExternalID != "SUP-42" {
		t.Fatalf("external id = %q, want SUP-42", iss.ExternalID)
	}
	if gotMethod != http.MethodPost || gotPath != "/rest/api/3/issue" {
		t.Fatalf("request = %s %s, want POST /rest/api/3/issue", gotMethod, gotPath)
	}
	if gotAuth == "" {
		t.Fatalf("missing basic auth")
	}
	fields, _ := gotBody["fields"].(map[string]any)
	if fields == nil || fields["summary"] != "Login broken" {
		t.Fatalf("fields summary wrong: %+v", gotBody)
	}
	proj, _ := fields["project"].(map[string]any)
	if proj == nil || proj["key"] != "SUP" {
		t.Fatalf("project key wrong: %+v", fields)
	}
}

func TestCreateIssueRejectsEmptyProject(t *testing.T) {
	c := newTestClient("https://acme.atlassian.net", "ops@acme.test", "tok", "whsec", http.DefaultClient)
	_, err := c.CreateIssue(context.Background(), connectors.ExternalIssueDraft{IssueType: "Task", Summary: "x"})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream for empty project key", err)
	}
}

func TestCreateIssueOmitsEmptyDescription(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadGolden(t, "create_issue_response.json"))
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "ops@acme.test", "tok", "whsec", srv.Client())
	if _, err := c.CreateIssue(context.Background(), connectors.ExternalIssueDraft{
		ProjectKey: "SUP", IssueType: "Task", Summary: "no desc",
	}); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	fields, _ := gotBody["fields"].(map[string]any)
	if _, ok := fields["description"]; ok {
		t.Fatalf("description should be omitted when empty, got: %+v", fields)
	}
}

// ── Factory ───────────────────────────────────────────────────────────────────

func TestNewFactory_ReturnsNonNil(t *testing.T) {
	f := NewFactory(10 * time.Second)
	if f == nil {
		t.Fatal("NewFactory returned nil")
	}
}

func TestNewFactory_BuildsClient(t *testing.T) {
	f := NewFactory(10 * time.Second)
	rc := resolvedConnector("https://mycompany.atlassian.net", "user@example.com", "my-token", "my-secret", false)
	conn, err := f(rc)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if conn == nil {
		t.Fatal("factory returned nil connector")
	}
}

func TestNewFactory_MissingBaseURL(t *testing.T) {
	f := NewFactory(10 * time.Second)
	rc := resolvedConnector("", "user@example.com", "token", "", false)
	_, err := f(rc)
	if err == nil {
		t.Fatal("expected error for missing base_url, got nil")
	}
}

func TestNewFactory_MissingCredentials(t *testing.T) {
	f := NewFactory(10 * time.Second)
	rc := resolvedConnector("https://mycompany.atlassian.net", "", "", "", false)
	_, err := f(rc)
	if err == nil {
		t.Fatal("expected error for missing credentials, got nil")
	}
}
