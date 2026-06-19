package jira

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/connectors"
)

// roundTripFunc adapts a function to http.RoundTripper so a test can force httpClient.Do to fail.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestDoJSONLogsTransportCause pins manyforge-zci: a transport failure (network error, timeout,
// netsafe SSRF dial-refusal, firewall block) collapses to the opaque ErrUnreachable sentinel,
// which callers MUST keep seeing (Principle II — never leak library/network internals). But the
// REAL underlying cause must be logged server-side so an operator can tell a firewall block from
// a genuine upstream outage (the 2026-06-12 Little Snitch incident: identical "service
// unreachable" log lines with zero pointer to the firewall).
func TestDoJSONLogsTransportCause(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })

	const cause = "dial tcp 13.227.180.4:443: connect: blocked by firewall"
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(cause)
	})}
	c := newTestClient("https://acme.atlassian.net", "agent@example.com", "tok", "secret", hc)

	_, err := c.FetchIssue(context.Background(), "JIRA-1")

	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	// The cause must be logged server-side so the failure is diagnosable...
	if !strings.Contains(buf.String(), "blocked by firewall") {
		t.Fatalf("transport cause was not logged; log output: %s", buf.String())
	}
	// ...but must NOT leak into the error returned to callers (Principle II).
	if strings.Contains(err.Error(), "blocked by firewall") {
		t.Fatalf("returned error leaked the underlying cause: %v", err)
	}
}

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
			// The fields query MUST request the issue description — otherwise a
			// description-only issue syncs with no body (the bug this fixes).
			if q := r.URL.Query().Get("fields"); q != "summary,status,priority,reporter,updated,description" {
				t.Errorf("FetchIssue: fields query = %q, want it to include description", q)
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
	// The ADF description body must be extracted to plain text and surfaced as
	// ExternalIssue.Description (synced downstream as the first inbound message).
	if !strings.Contains(issue.Description, "tap the login button") {
		t.Errorf("Description = %q, want extracted ADF text containing 'tap the login button'", issue.Description)
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

// TestFetchIssue_MissingDescription asserts a null/absent description decodes to an empty
// Description (no body) rather than erroring — so the downstream "first inbound message"
// sync is simply skipped for issues with no description.
func TestFetchIssue_MissingDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/comment"):
			_, _ = w.Write([]byte(`{"comments":[]}`))
		default:
			// description is null (a real Jira shape for an issue with no body).
			_, _ = w.Write([]byte(`{"key":"PROJ-9","fields":{"summary":"no body","status":{"name":"Open"},"priority":{"name":"Low"},"reporter":{"emailAddress":"x@y.z","displayName":"X"},"description":null,"updated":"2026-06-01T10:30:00.000+0000"}}`))
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "user@example.com", "tok", "secret", srv.Client())
	issue, err := c.FetchIssue(context.Background(), "PROJ-9")
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if issue.Description != "" {
		t.Errorf("Description = %q, want empty for a null description", issue.Description)
	}
}

// ── PostComment (internal vs public) ─────────────────────────────────────────

// TestPostComment_Internal asserts that PostComment(..., internal=true) attaches the JSM
// sd.public.comment property with internal=true so the comment is agent-only (not visible
// to the requester). This is the core of manyforge-8c4.
func TestPostComment_Internal(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"30001","author":{"displayName":"Agent"},"created":"2026-06-01T10:30:00.000+0000"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "agent@example.com", "tok", "secret", srv.Client())
	cm, err := c.PostComment(context.Background(), "PROJ-42", "internal triage note", true)
	if err != nil {
		t.Fatalf("PostComment internal: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/rest/api/3/issue/PROJ-42/comment" {
		t.Errorf("request = %s %s, want POST /rest/api/3/issue/PROJ-42/comment", gotMethod, gotPath)
	}
	// Decode the body and assert the JSM internal-comment property is present + internal=true.
	var payload struct {
		Properties []struct {
			Key   string `json:"key"`
			Value struct {
				Internal bool `json:"internal"`
			} `json:"value"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("unmarshal body: %v (body=%s)", err, gotBody)
	}
	if len(payload.Properties) != 1 {
		t.Fatalf("internal=true must carry exactly one property, body: %s", gotBody)
	}
	if payload.Properties[0].Key != "sd.public.comment" || !payload.Properties[0].Value.Internal {
		t.Errorf("property = %+v, want key=sd.public.comment value.internal=true", payload.Properties[0])
	}
	if cm.ExternalID != "30001" {
		t.Errorf("ExternalID = %q, want 30001", cm.ExternalID)
	}
}

// TestPostComment_Public asserts that PostComment(..., internal=false) does NOT attach the
// sd.public.comment property, so the comment posts as a normal (requester-visible) reply.
func TestPostComment_Public(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"30002","author":{"displayName":"Agent"},"created":"2026-06-01T10:30:00.000+0000"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "agent@example.com", "tok", "secret", srv.Client())
	if _, err := c.PostComment(context.Background(), "PROJ-42", "public reply", false); err != nil {
		t.Fatalf("PostComment public: %v", err)
	}
	if strings.Contains(gotBody, "sd.public.comment") || strings.Contains(gotBody, "properties") {
		t.Errorf("internal=false comment must NOT carry the JSM internal property, body: %s", gotBody)
	}
}

// ── TransitionStatus ─────────────────────────────────────────────────────────

// jiraTransitionsRouter builds a test server emulating the two transition endpoints plus the
// lightweight ?fields=status issue GET. transitions is the JSON array body returned by
// GET .../transitions; currentStatus is the status name returned by GET .../issue/{key}.
// It records whether a transition POST happened and the id it carried.
func jiraTransitionsRouter(t *testing.T, transitions, currentStatus string, posted *bool, postedID *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/transitions") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"transitions":` + transitions + `}`))
		case strings.HasSuffix(r.URL.Path, "/transitions") && r.Method == http.MethodPost:
			*posted = true
			var body struct {
				Transition struct {
					ID string `json:"id"`
				} `json:"transition"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			*postedID = body.Transition.ID
			w.WriteHeader(http.StatusNoContent)
		default: // GET .../issue/{key}?fields=status
			_, _ = w.Write([]byte(`{"key":"PROJ-1","fields":{"status":{"name":"` + currentStatus + `"}}}`))
		}
	}))
}

// TestTransitionStatus_Success pins the existing happy path: a transition whose target status
// matches is POSTed by id. (Characterization — must keep passing through the idempotency fix.)
func TestTransitionStatus_Success(t *testing.T) {
	var posted bool
	var postedID string
	srv := jiraTransitionsRouter(t,
		`[{"id":"21","name":"Start Progress","to":{"name":"In Progress"}},{"id":"31","name":"Resolve","to":{"name":"Done"}}]`,
		"In Progress", &posted, &postedID)
	defer srv.Close()

	c := newTestClient(srv.URL, "agent@example.com", "tok", "secret", srv.Client())
	if err := c.TransitionStatus(context.Background(), "PROJ-1", "Done"); err != nil {
		t.Fatalf("TransitionStatus: %v", err)
	}
	if !posted {
		t.Fatal("expected a transition POST, none happened")
	}
	if postedID != "31" {
		t.Errorf("posted transition id = %q, want 31", postedID)
	}
}

// TestTransitionStatus_AlreadyInTargetStatus_NoOp pins manyforge-zal: Jira lists only the
// transitions valid FROM the current status, so an issue ALREADY in the target status has no
// transition that reaches it. That must be an idempotent no-op (return nil, no POST) rather than
// a hard failure — otherwise an agent re-issuing "→Done" on an already-Done issue creates a
// permanently-failed outbound op that pins the connector to "degraded" (manyforge-xfj).
func TestTransitionStatus_AlreadyInTargetStatus_NoOp(t *testing.T) {
	var posted bool
	var postedID string
	// No transition reaches "Done", and the issue is already in "Done".
	srv := jiraTransitionsRouter(t,
		`[{"id":"21","name":"Start Progress","to":{"name":"In Progress"}}]`,
		"Done", &posted, &postedID)
	defer srv.Close()

	c := newTestClient(srv.URL, "agent@example.com", "tok", "secret", srv.Client())
	if err := c.TransitionStatus(context.Background(), "PROJ-1", "Done"); err != nil {
		t.Fatalf("already-in-target TransitionStatus must be a no-op, got error: %v", err)
	}
	if posted {
		t.Error("no transition POST should happen when the issue is already in the target status")
	}
}

// TestTransitionStatus_NoTransitionDifferentStatus_StaysLoud guards the boundary of the
// idempotency fix: a genuine misconfiguration (no transition reaches the target AND the issue is
// NOT already there — e.g. the workflow names it "Resolved", not "Done") must stay a loud
// ErrUpstream error, never be silently swallowed.
func TestTransitionStatus_NoTransitionDifferentStatus_StaysLoud(t *testing.T) {
	var posted bool
	var postedID string
	srv := jiraTransitionsRouter(t,
		`[{"id":"21","name":"Start Progress","to":{"name":"In Progress"}}]`,
		"In Progress", &posted, &postedID)
	defer srv.Close()

	c := newTestClient(srv.URL, "agent@example.com", "tok", "secret", srv.Client())
	err := c.TransitionStatus(context.Background(), "PROJ-1", "Done")
	if err == nil {
		t.Fatal("expected a loud error when no transition reaches the target and the issue is not already there")
	}
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("err = %v, want wrapped ErrUpstream", err)
	}
	if posted {
		t.Error("no transition POST should happen when no matching transition exists")
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
		// manyforge-7kf: a non-zero `since` MUST produce a timezone-independent relative
		// bound (updated >= "-Nm"), never a UTC absolute datetime. Jira reads a bare datetime
		// literal in the account's timezone, so a UTC absolute silently drops issues for
		// accounts behind UTC — the incremental sweep then returns nothing forever.
		if !strings.Contains(jql, `updated >= "-`) || !strings.Contains(jql, `m"`) {
			t.Errorf("ListUpdatedSince: jql = %q, want relative bound like updated >= \"-Nm\"", jql)
		}
		if strings.Contains(jql, "2026-06-01") {
			t.Errorf("ListUpdatedSince: jql = %q must NOT embed a UTC absolute date (TZ bug)", jql)
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

// manyforge-7kf: the full-pull path (since == zero, used by "Sync now") must keep an
// ancient ABSOLUTE lower bound so it fetches everything regardless of timezone — only the
// incremental path uses the relative "-Nm" form. Guards the path that currently works.
func TestListUpdatedSince_FullPullUsesAbsoluteBound(t *testing.T) {
	searchBody := loadGolden(t, "search_updated.json")
	var gotJQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotJQL = r.URL.Query().Get("jql")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(searchBody)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "user@example.com", "test-api-token", "secret", srv.Client())
	if _, err := c.ListUpdatedSince(context.Background(), time.Time{}); err != nil {
		t.Fatalf("ListUpdatedSince(zero): %v", err)
	}
	if !strings.Contains(gotJQL, "1970-01-01") {
		t.Errorf("full-pull jql = %q, want ancient absolute bound (1970-01-01)", gotJQL)
	}
	if strings.Contains(gotJQL, `"-`) {
		t.Errorf("full-pull jql = %q must NOT use a relative bound", gotJQL)
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

// a7j.12: a pathological upstream that always returns a non-empty page plus a fresh
// nextPageToken never signals "last page". The absolute page cap must bound the loop.
func TestListUpdatedSince_StopsAtPageCap(t *testing.T) {
	orig := maxSearchPages
	maxSearchPages = 3
	defer func() { maxSearchPages = orig }()

	var reqs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqs++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[{"key":"PROJ-1","fields":{"updated":"2026-06-01T10:30:00.000+0000"}}],"nextPageToken":"more"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "user@example.com", "test-api-token", "secret", srv.Client())
	keys, err := c.ListUpdatedSince(context.Background(), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListUpdatedSince: %v", err)
	}
	if reqs != 3 {
		t.Errorf("made %d requests, want exactly maxSearchPages=3 (loop must be bounded)", reqs)
	}
	if len(keys) != 3 {
		t.Errorf("collected %d keys, want 3 (one per capped page)", len(keys))
	}
}

// A connector with a configured project_key must scope the JQL to that project so
// inbound reconcile does not pull every project the token can see.
func TestListUpdatedSince_ScopedToProject(t *testing.T) {
	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	var gotJQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotJQL = r.URL.Query().Get("jql")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[{"key":"MT-1","fields":{"updated":"2026-06-01T10:30:00.000+0000"}}]}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "user@example.com", "test-api-token", "secret", srv.Client())
	c.projectKey = "MT"
	keys, err := c.ListUpdatedSince(context.Background(), since)
	if err != nil {
		t.Fatalf("ListUpdatedSince: %v", err)
	}
	if !strings.Contains(gotJQL, `project = "MT"`) {
		t.Errorf("jql = %q, want a `project = \"MT\"` clause", gotJQL)
	}
	if len(keys) != 1 || keys[0] != "MT-1" {
		t.Errorf("keys = %v, want [MT-1]", keys)
	}
}

// ── VerifyWebhook ────────────────────────────────────────────────────────────

// VerifyAuth probes GET /rest/api/3/myself; 2xx → nil, non-2xx (bad credential) → ErrUpstream.
func TestVerifyAuth(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accountId":"abc","emailAddress":"a@b.c"}`))
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "user@example.com", "tok", "secret", srv.Client())
	if err := c.VerifyAuth(context.Background()); err != nil {
		t.Fatalf("VerifyAuth: %v", err)
	}
	if gotPath != "/rest/api/3/myself" {
		t.Errorf("path = %q, want /rest/api/3/myself", gotPath)
	}
}

func TestVerifyAuth_RejectsBadCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "user@example.com", "bad", "secret", srv.Client())
	if err := c.VerifyAuth(context.Background()); !errors.Is(err, ErrUpstream) {
		t.Fatalf("VerifyAuth on 401 = %v, want ErrUpstream", err)
	}
}

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
	if _, err := c.PostComment(context.Background(), "PROJ-1/../admin", "hi", false); !errors.Is(err, ErrBadIssueKey) {
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
