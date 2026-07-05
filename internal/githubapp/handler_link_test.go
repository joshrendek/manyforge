package githubapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/httpx"
)

type linkRec struct {
	install    int64
	biz, agent uuid.UUID
	called     bool
	err        error
}

func (r *linkRec) UpsertFromEvent(ctx context.Context, id int64, l, a string) error { return nil }
func (r *linkRec) MarkDeleted(ctx context.Context, id int64) error                  { return nil }
func (r *linkRec) SetSuspended(ctx context.Context, id int64, s bool) error         { return nil }
func (r *linkRec) Link(ctx context.Context, id int64, biz, agent uuid.UUID) error {
	r.install, r.biz, r.agent, r.called = id, biz, agent, true
	return r.err
}

type stubPerms struct{ ok bool }

func (p *stubPerms) Has(ctx context.Context, pr, b uuid.UUID, perm string) (bool, error) {
	return p.ok, nil
}

func linkHandler(api GitHubAPI, installs installOps, perms permChecker, nonceFirst bool) (*Handler, []byte) {
	key := []byte("0123456789abcdef0123456789abcdef")
	return &Handler{API: api, Installs: installs, Perms: perms, Nonces: &stubNonce{first: nonceFirst},
		Store: &stubStore{cfg: AppConfig{ClientID: "cid", ClientSecret: "sec"}}, StateKey: key,
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}, key
}

func linkReq(state string) *http.Request {
	body := strings.NewReader(`{"code":"oc","installation_id":"555","state":"` + state + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/github/app/installations/link", body)
	// caller is authenticated:
	return req.WithContext(httpx.WithPrincipal(req.Context(), uuid.New()))
}

func TestLinkRequiresOAuthProof(t *testing.T) {
	biz, agent := uuid.New(), uuid.New()
	rec := &linkRec{}
	h, key := linkHandler(&fakeAPI{userInstalls: []Installation{}}, rec, &stubPerms{ok: true}, true) // controls NO installs
	state := signState(key, StatePayload{Purpose: "link", BusinessID: biz, AgentID: agent, Nonce: "n", Exp: h.Now().Add(time.Minute).Unix()})
	r := chi.NewRouter()
	h.LinkRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, linkReq(state))
	if w.Code == http.StatusOK || rec.called {
		t.Fatalf("linked without proof (status %d, called %v)", w.Code, rec.called)
	}
}

func TestLinkRejectsCallerLackingPermission(t *testing.T) {
	biz, agent := uuid.New(), uuid.New()
	rec := &linkRec{}
	h, key := linkHandler(&fakeAPI{userInstalls: []Installation{{ID: 555}}}, rec, &stubPerms{ok: false}, true) // not a member
	state := signState(key, StatePayload{Purpose: "link", BusinessID: biz, AgentID: agent, Nonce: "n", Exp: h.Now().Add(time.Minute).Unix()})
	r := chi.NewRouter()
	h.LinkRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, linkReq(state))
	if w.Code != http.StatusNotFound || rec.called {
		t.Fatalf("non-member linked (status %d, called %v)", w.Code, rec.called)
	}
}

func TestLinkSucceedsWithProofAndPermission(t *testing.T) {
	biz, agent := uuid.New(), uuid.New()
	rec := &linkRec{}
	h, key := linkHandler(&fakeAPI{userInstalls: []Installation{{ID: 555, Login: "bluescripts-net", Type: "Organization"}}}, rec, &stubPerms{ok: true}, true)
	state := signState(key, StatePayload{Purpose: "link", BusinessID: biz, AgentID: agent, Nonce: "n", Exp: h.Now().Add(time.Minute).Unix()})
	r := chi.NewRouter()
	h.LinkRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, linkReq(state))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !rec.called || rec.install != 555 || rec.biz != biz || rec.agent != agent {
		t.Fatalf("Link got (%d,%v,%v)", rec.install, rec.biz, rec.agent)
	}
}

func TestLinkRejectsReplayedNonce(t *testing.T) {
	biz, agent := uuid.New(), uuid.New()
	rec := &linkRec{}
	h, key := linkHandler(&fakeAPI{userInstalls: []Installation{{ID: 555}}}, rec, &stubPerms{ok: true}, false) // nonce already used
	state := signState(key, StatePayload{Purpose: "link", BusinessID: biz, AgentID: agent, Nonce: "n", Exp: h.Now().Add(time.Minute).Unix()})
	r := chi.NewRouter()
	h.LinkRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, linkReq(state))
	if w.Code == http.StatusOK || rec.called {
		t.Fatalf("replayed nonce accepted (status %d)", w.Code)
	}
}
