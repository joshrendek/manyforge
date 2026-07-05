package githubapp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const webhookSecret = "whsec"

// --- exact JSON payloads ---

const openedPRPayload = `{
  "action": "opened",
  "number": 42,
  "installation": {"id": 9},
  "repository": {"full_name": "acme/widgets"},
  "pull_request": {
    "draft": false,
    "user": {"type": "User"},
    "head": {"sha": "abc123", "repo": {"id": 555}},
    "base": {"repo": {"id": 555}}
  }
}`

const draftPRPayload = `{
  "action": "opened",
  "number": 42,
  "installation": {"id": 9},
  "repository": {"full_name": "acme/widgets"},
  "pull_request": {
    "draft": true,
    "user": {"type": "User"},
    "head": {"sha": "abc123", "repo": {"id": 555}},
    "base": {"repo": {"id": 555}}
  }
}`

const botAuthorPRPayload = `{
  "action": "opened",
  "number": 42,
  "installation": {"id": 9},
  "repository": {"full_name": "acme/widgets"},
  "pull_request": {
    "draft": false,
    "user": {"type": "Bot"},
    "head": {"sha": "abc123", "repo": {"id": 555}},
    "base": {"repo": {"id": 555}}
  }
}`

const forkPRPayload = `{
  "action": "opened",
  "number": 42,
  "installation": {"id": 9},
  "repository": {"full_name": "acme/widgets"},
  "pull_request": {
    "draft": false,
    "user": {"type": "User"},
    "head": {"sha": "abc123", "repo": {"id": 777}},
    "base": {"repo": {"id": 555}}
  }
}`

const forkNullHeadRepoPRPayload = `{
  "action": "opened",
  "number": 42,
  "installation": {"id": 9},
  "repository": {"full_name": "acme/widgets"},
  "pull_request": {
    "draft": false,
    "user": {"type": "User"},
    "head": {"sha": "abc123"},
    "base": {"repo": {"id": 555}}
  }
}`

const nonTriggerActionPRPayload = `{
  "action": "closed",
  "number": 42,
  "installation": {"id": 9},
  "repository": {"full_name": "acme/widgets"},
  "pull_request": {
    "draft": false,
    "user": {"type": "User"},
    "head": {"sha": "abc123", "repo": {"id": 555}},
    "base": {"repo": {"id": 555}}
  }
}`

const malformedRepoFullNamePRPayload = `{
  "action": "opened",
  "number": 42,
  "installation": {"id": 9},
  "repository": {"full_name": "widgets"},
  "pull_request": {
    "draft": false,
    "user": {"type": "User"},
    "head": {"sha": "abc123", "repo": {"id": 555}},
    "base": {"repo": {"id": 555}}
  }
}`

const malformedEmptyHeadSHAPRPayload = `{
  "action": "opened",
  "number": 42,
  "installation": {"id": 9},
  "repository": {"full_name": "acme/widgets"},
  "pull_request": {
    "draft": false,
    "user": {"type": "User"},
    "head": {"sha": "", "repo": {"id": 555}},
    "base": {"repo": {"id": 555}}
  }
}`

// --- fake prReviewOps ---

type fakePRReviews struct {
	resolveCtx InstallationContext
	resolveOK  bool
	resolveErr error

	ingestID  uuid.UUID
	ingestOK  bool
	ingestErr error

	resolveCalls []int64
	ingestCalls  []PRReviewInput
}

func (f *fakePRReviews) ResolveInstallation(ctx context.Context, installationID int64) (InstallationContext, bool, error) {
	f.resolveCalls = append(f.resolveCalls, installationID)
	return f.resolveCtx, f.resolveOK, f.resolveErr
}

func (f *fakePRReviews) IngestPRReview(ctx context.Context, in PRReviewInput) (uuid.UUID, bool, error) {
	f.ingestCalls = append(f.ingestCalls, in)
	id := f.ingestID
	if id == uuid.Nil && f.ingestOK {
		id = uuid.New()
	}
	return id, f.ingestOK, f.ingestErr
}

// linkedEnabledContext is a fully linked, enabled, non-suspended installation
// context — the "should ingest" case.
func linkedEnabledContext() InstallationContext {
	return InstallationContext{
		BusinessID:       uuid.New(),
		TenantRootID:     uuid.New(),
		AgentID:          uuid.New(),
		AgentPrincipalID: uuid.New(),
		AgentEnabled:     true,
		Enabled:          true,
		Suspended:        false,
	}
}

// --- test harness ---

func newPRWebhookHandler(prReviews prReviewOps) (*Handler, chi.Router) {
	h := &Handler{
		Store:     &stubStore{cfg: AppConfig{AppID: 5, WebhookSecret: webhookSecret}},
		Installs:  &recordInstalls{},
		PRReviews: prReviews,
		Now:       func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	r := chi.NewRouter()
	h.WebhookRoutes(r)
	return h, r
}

func postPRWebhook(t *testing.T, r chi.Router, body []byte, deliveryID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "5")
	req.Header.Set("X-Hub-Signature-256", sign256([]byte(webhookSecret), body))
	if deliveryID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// --- filter matrix: no ingest call, uniform 202 ---

func TestHandlePullRequestFilteredEventsNeverIngest(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"draft", draftPRPayload},
		{"bot author", botAuthorPRPayload},
		{"fork (head.repo.id != base.repo.id)", forkPRPayload},
		{"fork (head.repo missing)", forkNullHeadRepoPRPayload},
		{"non-trigger action", nonTriggerActionPRPayload},
		{"malformed repository.full_name (no slash)", malformedRepoFullNamePRPayload},
		{"malformed empty head sha", malformedEmptyHeadSHAPRPayload},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakePRReviews{resolveCtx: linkedEnabledContext(), resolveOK: true, ingestOK: true}
			_, r := newPRWebhookHandler(fake)
			w := postPRWebhook(t, r, []byte(c.body), "delivery-"+c.name)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", w.Code)
			}
			if len(fake.resolveCalls) != 0 {
				t.Fatalf("ResolveInstallation called %d times, want 0", len(fake.resolveCalls))
			}
			if len(fake.ingestCalls) != 0 {
				t.Fatalf("IngestPRReview called %d times, want 0", len(fake.ingestCalls))
			}
		})
	}
}

// --- installation-context matrix: resolved but not ingestable ---

func TestHandlePullRequestSkipsUnlinkedOrDisabledInstallation(t *testing.T) {
	cases := []struct {
		name string
		ctx  InstallationContext
		ok   bool
	}{
		{"installation not found", InstallationContext{}, false},
		{"found but unlinked (zero business/agent)", InstallationContext{Enabled: true, AgentEnabled: true}, true},
		{"suspended", func() InstallationContext {
			c := linkedEnabledContext()
			c.Suspended = true
			return c
		}(), true},
		{"connector disabled", func() InstallationContext {
			c := linkedEnabledContext()
			c.Enabled = false
			return c
		}(), true},
		{"agent disabled", func() InstallationContext {
			c := linkedEnabledContext()
			c.AgentEnabled = false
			return c
		}(), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakePRReviews{resolveCtx: c.ctx, resolveOK: c.ok, ingestOK: true}
			_, r := newPRWebhookHandler(fake)
			w := postPRWebhook(t, r, []byte(openedPRPayload), "delivery-1")
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", w.Code)
			}
			if len(fake.resolveCalls) != 1 || fake.resolveCalls[0] != 9 {
				t.Fatalf("resolveCalls = %v, want [9]", fake.resolveCalls)
			}
			if len(fake.ingestCalls) != 0 {
				t.Fatalf("IngestPRReview called %d times, want 0", len(fake.ingestCalls))
			}
		})
	}
}

// --- happy path ---

func TestHandlePullRequestOpenedResolvesAndIngests(t *testing.T) {
	linked := linkedEnabledContext()
	fake := &fakePRReviews{resolveCtx: linked, resolveOK: true, ingestOK: true}
	_, r := newPRWebhookHandler(fake)
	w := postPRWebhook(t, r, []byte(openedPRPayload), "delivery-123")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if len(fake.resolveCalls) != 1 || fake.resolveCalls[0] != 9 {
		t.Fatalf("resolveCalls = %v, want [9]", fake.resolveCalls)
	}
	if len(fake.ingestCalls) != 1 {
		t.Fatalf("IngestPRReview called %d times, want 1", len(fake.ingestCalls))
	}
	in := fake.ingestCalls[0]
	if in.InstallationID != 9 {
		t.Errorf("InstallationID = %d, want 9", in.InstallationID)
	}
	if in.DeliveryID != "delivery-123" {
		t.Errorf("DeliveryID = %q, want %q", in.DeliveryID, "delivery-123")
	}
	if in.Repo != "acme/widgets" {
		t.Errorf("Repo = %q, want acme/widgets", in.Repo)
	}
	if in.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", in.PRNumber)
	}
	if in.HeadSHA != "abc123" {
		t.Errorf("HeadSHA = %q, want abc123", in.HeadSHA)
	}
	if in.BusinessID != linked.BusinessID || in.TenantRootID != linked.TenantRootID ||
		in.AgentID != linked.AgentID || in.AgentPrincipalID != linked.AgentPrincipalID {
		t.Errorf("resolved business/agent fields not passed through: %+v vs context %+v", in, linked)
	}
}

// ok=false from IngestPRReview (replay/rate/dup) is not a caller error and
// still returns 202 with no panic.
func TestHandlePullRequestIngestNoOpStillAccepted(t *testing.T) {
	fake := &fakePRReviews{resolveCtx: linkedEnabledContext(), resolveOK: true, ingestOK: false}
	_, r := newPRWebhookHandler(fake)
	w := postPRWebhook(t, r, []byte(openedPRPayload), "delivery-dup")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if len(fake.ingestCalls) != 1 {
		t.Fatalf("IngestPRReview called %d times, want 1", len(fake.ingestCalls))
	}
}

// A nil PRReviews (webhook wired without the enqueuer) must no-op, not panic.
func TestHandlePullRequestNilPRReviewsNoPanic(t *testing.T) {
	_, r := newPRWebhookHandler(nil)
	w := postPRWebhook(t, r, []byte(openedPRPayload), "delivery-1")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
}

// Bad signature still 401s for a pull_request event (reuses the Slice-1
// signature-check path — it runs before event-type dispatch).
func TestHandlePullRequestBadSignatureRejected(t *testing.T) {
	fake := &fakePRReviews{resolveCtx: linkedEnabledContext(), resolveOK: true, ingestOK: true}
	_, r := newPRWebhookHandler(fake)
	req := httptest.NewRequest(http.MethodPost, "/github/webhook", bytes.NewReader([]byte(openedPRPayload)))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Hook-Installation-Target-ID", "5")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if len(fake.resolveCalls) != 0 || len(fake.ingestCalls) != 0 {
		t.Fatalf("resolve/ingest called on bad signature: resolve=%v ingest=%v", fake.resolveCalls, fake.ingestCalls)
	}
}
