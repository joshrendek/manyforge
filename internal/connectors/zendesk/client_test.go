package zendesk

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
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

// TestDoJSONLogsTransportCause pins manyforge-zci: a transport failure collapses to the opaque
// ErrUnreachable sentinel (Principle II — callers never see network/library internals), but the
// real underlying cause must be logged server-side so a firewall block is distinguishable from a
// genuine upstream outage.
func TestDoJSONLogsTransportCause(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(old) })

	const cause = "dial tcp 104.16.51.111:443: connect: blocked by firewall"
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(cause)
	})}
	c := newTestClient("https://acme.zendesk.com", "agent@example.com", "tok", "secret", hc)

	_, err := c.FetchIssue(context.Background(), "123")

	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if !strings.Contains(buf.String(), "blocked by firewall") {
		t.Fatalf("transport cause was not logged; log output: %s", buf.String())
	}
	if strings.Contains(err.Error(), "blocked by firewall") {
		t.Fatalf("returned error leaked the underlying cause: %v", err)
	}
}

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
		switch r.URL.Path {
		case "/api/v2/tickets/12345.json":
			sawInclude = r.URL.Query().Get("include")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(mustFixture(t, "ticket.json"))
		case "/api/v2/tickets/12345/comments.json":
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

func TestPostComment(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustFixture(t, "post_comment_response.json"))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "ops@acme.test", "tok-123", "secret", srv.Client())
	cm, err := c.PostComment(context.Background(), "12345", "Reply from ManyForge", false)
	if err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v2/tickets/12345.json" {
		t.Errorf("request = %s %s, want PUT /api/v2/tickets/12345.json", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"comment"`) || !strings.Contains(gotBody, "Reply from ManyForge") {
		t.Errorf("body did not carry the comment: %s", gotBody)
	}
	// internal=false ⇒ a PUBLIC (requester-visible) comment.
	if !strings.Contains(gotBody, `"public":true`) {
		t.Errorf("internal=false comment must be public:true, body: %s", gotBody)
	}
	if cm.ExternalID != "8001" {
		t.Errorf("ExternalID = %q, want 8001 (audit Comment event id)", cm.ExternalID)
	}
	if cm.Body != "Reply from ManyForge" {
		t.Errorf("Body = %q", cm.Body)
	}
}

func TestPostComment_RejectsTraversalID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be reached for an invalid ticket id")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "ops@acme.test", "tok-123", "secret", srv.Client())
	if _, err := c.PostComment(context.Background(), "9/../1", "hi", false); !errors.Is(err, ErrBadTicketID) {
		t.Fatalf("err = %v, want Is(ErrBadTicketID)", err)
	}
}

func TestCreateIssue(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustFixture(t, "create_ticket_response.json"))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "ops@acme.test", "tok-123", "secret", srv.Client())
	issue, err := c.CreateIssue(context.Background(), connectors.ExternalIssueDraft{
		Summary: "Escalated from ManyForge", Description: "Customer cannot log in", IssueType: "Task",
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v2/tickets.json" {
		t.Errorf("request = %s %s, want POST /api/v2/tickets.json", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"subject":"Escalated from ManyForge"`) ||
		!strings.Contains(gotBody, "Customer cannot log in") || !strings.Contains(gotBody, `"type":"task"`) {
		t.Errorf("create body wrong: %s", gotBody)
	}
	if issue.ExternalID != "67890" {
		t.Errorf("ExternalID = %q, want 67890", issue.ExternalID)
	}
	if !strings.HasSuffix(issue.URL, "/agent/tickets/67890") {
		t.Errorf("URL = %q", issue.URL)
	}
}

func TestCreateIssue_RequiresSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be reached without a summary")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "ops@acme.test", "tok-123", "secret", srv.Client())
	if _, err := c.CreateIssue(context.Background(), connectors.ExternalIssueDraft{}); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want Is(ErrUpstream) for empty summary", err)
	}
}

func TestTransitionStatus(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ticket":{"id":12345,"status":"solved"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "ops@acme.test", "tok-123", "secret", srv.Client())
	if err := c.TransitionStatus(context.Background(), "12345", "Solved"); err != nil {
		t.Fatalf("TransitionStatus: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v2/tickets/12345.json" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"status":"solved"`) {
		t.Errorf("body = %s, want lowercased status", gotBody)
	}
}

func TestTransitionStatus_RejectsUnknownStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be reached for an unknown status")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "ops@acme.test", "tok-123", "secret", srv.Client())
	if err := c.TransitionStatus(context.Background(), "12345", "frozen"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want Is(ErrUpstream) for unknown status", err)
	}
}

func TestTransitionStatus_RejectsTraversalID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be reached for an invalid ticket id")
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "ops@acme.test", "tok-123", "secret", srv.Client())
	if err := c.TransitionStatus(context.Background(), "../../admin", "open"); !errors.Is(err, ErrBadTicketID) {
		t.Fatalf("err = %v, want Is(ErrBadTicketID)", err)
	}
}

func TestListUpdatedSince_Paginates(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		if got := r.URL.Query().Get("query"); !strings.Contains(got, "type:ticket updated>") {
			t.Errorf("query = %q, want type:ticket updated> filter", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "1":
			_, _ = w.Write(mustFixture(t, "search_page1.json"))
		case "2":
			_, _ = w.Write(mustFixture(t, "search_page2.json"))
		default:
			t.Errorf("unexpected page %q (must stop after count is covered)", page)
			_, _ = w.Write([]byte(`{"results":[],"count":3}`))
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "ops@acme.test", "tok-123", "secret", srv.Client())
	ids, err := c.ListUpdatedSince(context.Background(), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListUpdatedSince: %v", err)
	}
	want := []string{"101", "102", "103"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if strings.Join(pages, ",") != "1,2" {
		t.Errorf("pages requested = %v, want [1 2]", pages)
	}
}

// a7j.12: a pathological upstream that returns a full non-empty page with a perpetually
// inflated count never satisfies the natural termination (len(ids) >= count). The absolute
// page cap must bound the loop so reconcile can't spin forever.
func TestListUpdatedSince_StopsAtPageCap(t *testing.T) {
	orig := maxSearchPages
	maxSearchPages = 3
	defer func() { maxSearchPages = orig }()

	var reqs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reqs++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":1}],"count":1000000}`))
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, "ops@acme.test", "tok", "secret", srv.Client())
	ids, err := c.ListUpdatedSince(context.Background(), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListUpdatedSince: %v", err)
	}
	if reqs != 3 {
		t.Errorf("made %d requests, want exactly maxSearchPages=3 (loop must be bounded)", reqs)
	}
	if len(ids) != 3 {
		t.Errorf("collected %d ids, want 3 (one per capped page)", len(ids))
	}
}

// zendeskSig computes the header value Zendesk sends: base64(HMAC-SHA256(secret, ts+body)).
func zendeskSig(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyAuth probes GET /api/v2/users/me.json; 2xx → nil, non-2xx (bad credential) → ErrUpstream.
func TestVerifyAuth(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":1,"email":"a@b.c"}}`))
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "ops@acme.test", "tok", "secret", srv.Client())
	if err := c.VerifyAuth(context.Background()); err != nil {
		t.Fatalf("VerifyAuth: %v", err)
	}
	if gotPath != "/api/v2/users/me.json" {
		t.Errorf("path = %q, want /api/v2/users/me.json", gotPath)
	}
}

func TestVerifyAuth_RejectsBadCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL, "ops@acme.test", "bad", "secret", srv.Client())
	if err := c.VerifyAuth(context.Background()); !errors.Is(err, ErrUpstream) {
		t.Fatalf("VerifyAuth on 401 = %v, want ErrUpstream", err)
	}
}

func TestVerifyWebhook_Valid(t *testing.T) {
	secret := "whsec-123"
	ts := "1718000000"
	body := []byte(`{"id":"d1","type":"zen:event-type:ticket.updated","detail":{"id":"12345"}}`)
	c := newTestClient("https://acme.zendesk.com", "ops@acme.test", "tok", secret, nil)
	hdr := http.Header{
		"X-Zendesk-Webhook-Signature":           []string{zendeskSig(secret, ts, body)},
		"X-Zendesk-Webhook-Signature-Timestamp": []string{ts},
	}
	if err := c.VerifyWebhook(hdr, body); err != nil {
		t.Fatalf("VerifyWebhook valid: %v", err)
	}
}

func TestVerifyWebhook_Forged(t *testing.T) {
	secret := "whsec-123"
	ts := "1718000000"
	body := []byte(`{"detail":{"id":"12345"}}`)
	c := newTestClient("https://acme.zendesk.com", "ops@acme.test", "tok", secret, nil)
	good, err := base64.StdEncoding.DecodeString(zendeskSig(secret, ts, body))
	if err != nil {
		t.Fatalf("decode good sig: %v", err)
	}
	good[0] ^= 0xff // same length (32 bytes), wrong bytes
	hdr := http.Header{
		"X-Zendesk-Webhook-Signature":           []string{base64.StdEncoding.EncodeToString(good)},
		"X-Zendesk-Webhook-Signature-Timestamp": []string{ts},
	}
	if err := c.VerifyWebhook(hdr, body); !errors.Is(err, ErrBadSig) {
		t.Fatalf("forged: err = %v, want Is(ErrBadSig)", err)
	}
}

func TestVerifyWebhook_MissingPieces(t *testing.T) {
	c := newTestClient("https://acme.zendesk.com", "ops@acme.test", "tok", "whsec-123", nil)
	body := []byte(`{}`)
	// Missing signature header.
	if err := c.VerifyWebhook(http.Header{"X-Zendesk-Webhook-Signature-Timestamp": []string{"1"}}, body); !errors.Is(err, ErrBadSig) {
		t.Errorf("missing sig: err = %v, want ErrBadSig", err)
	}
	// Missing timestamp header.
	if err := c.VerifyWebhook(http.Header{"X-Zendesk-Webhook-Signature": []string{"AAAA"}}, body); !errors.Is(err, ErrBadSig) {
		t.Errorf("missing ts: err = %v, want ErrBadSig", err)
	}
}

func TestVerifyWebhook_EmptySecretRejected(t *testing.T) {
	// Fail closed: an unconfigured secret must never validate any signature.
	c := newTestClient("https://acme.zendesk.com", "ops@acme.test", "tok", "", nil)
	body := []byte(`{}`)
	hdr := http.Header{
		"X-Zendesk-Webhook-Signature":           []string{zendeskSig("", "1", body)},
		"X-Zendesk-Webhook-Signature-Timestamp": []string{"1"},
	}
	if err := c.VerifyWebhook(hdr, body); !errors.Is(err, ErrBadSig) {
		t.Fatalf("empty secret: err = %v, want Is(ErrBadSig)", err)
	}
}

func TestDecodeWebhook(t *testing.T) {
	c := newTestClient("https://acme.zendesk.com", "ops@acme.test", "tok", "secret", nil)
	ev, err := c.DecodeWebhook(mustFixture(t, "webhook_ticket_updated.json"))
	if err != nil {
		t.Fatalf("DecodeWebhook: %v", err)
	}
	if ev.DeliveryID != "01HXDELIVERY7" {
		t.Errorf("DeliveryID = %q, want 01HXDELIVERY7", ev.DeliveryID)
	}
	if ev.ExternalID != "12345" {
		t.Errorf("ExternalID = %q, want 12345", ev.ExternalID)
	}
	if ev.Kind != "issue.updated" {
		t.Errorf("Kind = %q, want issue.updated", ev.Kind)
	}
}

func TestDecodeWebhook_RejectsBadTicketID(t *testing.T) {
	c := newTestClient("https://acme.zendesk.com", "ops@acme.test", "tok", "secret", nil)
	body := []byte(`{"id":"d1","type":"zen:event-type:ticket.updated","detail":{"id":"../../etc/passwd"}}`)
	if _, err := c.DecodeWebhook(body); !errors.Is(err, ErrBadTicketID) {
		t.Fatalf("err = %v, want Is(ErrBadTicketID)", err)
	}
}

func TestDecodeWebhook_DeliveryIDFallback(t *testing.T) {
	c := newTestClient("https://acme.zendesk.com", "ops@acme.test", "tok", "secret", nil)
	body := []byte(`{"type":"zen:event-type:ticket.updated","time":"2026-06-09T12:00:00Z","detail":{"id":"12345"}}`)
	ev, err := c.DecodeWebhook(body)
	if err != nil {
		t.Fatalf("DecodeWebhook: %v", err)
	}
	if ev.DeliveryID != "12345:2026-06-09T12:00:00Z" {
		t.Errorf("DeliveryID = %q, want 12345:2026-06-09T12:00:00Z", ev.DeliveryID)
	}
}
