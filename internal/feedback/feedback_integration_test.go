//go:build integration

package feedback_test

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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/manyforge/manyforge/internal/feedback"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	appdb "github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

// fbSeed is a seeded tenant + owner principal authorizing RLS for the feedback tests.
// businessID doubles as tenant_root_id (a tenant-root business, parent_id NULL).
type fbSeed struct {
	businessID  uuid.UUID
	principalID uuid.UUID
}

// seedFeedbackTenant seeds account → principal → tenant-root business → closure self-row →
// owner membership, so authorized_businesses(current_principal()) returns this business and
// db.WithPrincipal authorizes it. Seeding runs as the RLS-exempt superuser.
func seedFeedbackTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) fbSeed {
	t.Helper()
	return seedFeedbackTenantPool(ctx, t, tdb.Super)
}

// seedFeedbackTenantPool is seedFeedbackTenant's implementation against a raw superuser pool
// rather than a *testdb.TestDB, so it can also seed
// TestFeedbackVerifiedIdentityBackfillMigration0104's version-capped test-local Postgres, which
// cannot use testdb.Start (testdb.Start always migrates straight to HEAD; see that test).
func seedFeedbackTenantPool(ctx context.Context, t *testing.T, super *pgxpool.Pool) fbSeed {
	t.Helper()
	var ownerRole uuid.UUID
	if err := super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}
	s := fbSeed{businessID: uuid.New(), principalID: uuid.New()}
	acctID := uuid.New()
	email := "fb-owner-" + s.businessID.String() + "@x.test"

	tx, err := super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{acctID, email}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{s.principalID, acctID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'FBCo','active',now(),now())`,
			[]any{s.businessID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{s.businessID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{s.principalID, s.businessID, ownerRole}},
	}
	for _, st := range stmts {
		if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, st.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return s
}

// TestBoardPostLifecycle exercises the authenticated surface: board create/get/update, internal
// post create/list/get, status moderation, and soft-delete.
func TestBoardPostLifecycle(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	pid, biz := seed.principalID, seed.businessID
	svc := &feedback.Service{DB: tdb.App}

	board, err := svc.CreateBoard(ctx, pid, biz, feedback.BoardInput{Name: "Feature Requests"})
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	if board.Slug != "feature-requests" {
		t.Fatalf("slug = %q, want feature-requests", board.Slug)
	}
	if board.IsPublic {
		t.Fatalf("new board should default private")
	}

	// Duplicate slug in the same business is a conflict.
	if _, err := svc.CreateBoard(ctx, pid, biz, feedback.BoardInput{Slug: "feature-requests", Name: "Dup"}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("duplicate slug err = %v, want ErrConflict", err)
	}

	// Make it public (partial update preserves name).
	desc := "public board"
	pub := true
	updated, err := svc.UpdateBoard(ctx, pid, biz, board.ID, feedback.BoardUpdate{Description: &desc, IsPublic: &pub})
	if err != nil {
		t.Fatalf("UpdateBoard: %v", err)
	}
	if !updated.IsPublic || updated.Name != "Feature Requests" {
		t.Fatalf("update: is_public=%v name=%q", updated.IsPublic, updated.Name)
	}

	post, err := svc.CreatePost(ctx, pid, biz, board.ID, feedback.PostInput{Title: "Dark mode"})
	if err != nil {
		t.Fatalf("CreatePost: %v", err)
	}
	if post.Status != "open" || post.AuthorKind != "principal" || post.AuthorPrincipalID == nil || *post.AuthorPrincipalID != pid {
		t.Fatalf("post = %+v", post)
	}

	page, err := svc.ListPosts(ctx, pid, biz, board.ID, "", 0)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("ListPosts = %+v err=%v", page.Items, err)
	}

	moved, err := svc.SetPostStatus(ctx, pid, biz, post.ID, "planned")
	if err != nil || moved.Status != "planned" {
		t.Fatalf("SetPostStatus = %+v err=%v", moved, err)
	}
	if _, err := svc.SetPostStatus(ctx, pid, biz, post.ID, "bogus"); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("bogus status err = %v, want ErrValidation", err)
	}

	if err := svc.DeletePost(ctx, pid, biz, post.ID); err != nil {
		t.Fatalf("DeletePost: %v", err)
	}
	if _, err := svc.GetPost(ctx, pid, biz, post.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Get after delete err = %v, want ErrNotFound", err)
	}
}

// TestVotingIntegrity asserts one vote per identity per post: a second vote by the same
// principal is a no-op and the count does not inflate.
func TestVotingIntegrity(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	pid, biz := seed.principalID, seed.businessID
	svc := &feedback.Service{DB: tdb.App}

	board, _ := svc.CreateBoard(ctx, pid, biz, feedback.BoardInput{Name: "B"})
	post, _ := svc.CreatePost(ctx, pid, biz, board.ID, feedback.PostInput{Title: "T"})

	voted, count, err := svc.Vote(ctx, pid, biz, post.ID)
	if err != nil || !voted || count != 1 {
		t.Fatalf("first vote: voted=%v count=%d err=%v", voted, count, err)
	}
	voted, count, err = svc.Vote(ctx, pid, biz, post.ID)
	if err != nil || voted || count != 1 {
		t.Fatalf("replay vote: voted=%v count=%d err=%v (want voted=false count=1)", voted, count, err)
	}
}

// TestTenantIsolation asserts a board in tenant B is invisible to a principal in tenant A —
// collapsing to ErrNotFound (no existence oracle), even though the id is valid.
func TestTenantIsolation(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	a := seedFeedbackTenant(ctx, t, tdb)
	b := seedFeedbackTenant(ctx, t, tdb)
	svc := &feedback.Service{DB: tdb.App}

	boardB, err := svc.CreateBoard(ctx, b.principalID, b.businessID, feedback.BoardInput{Name: "B secrets"})
	if err != nil {
		t.Fatalf("CreateBoard B: %v", err)
	}
	// Principal A asking for B's board under A's own business → not found.
	if _, err := svc.GetBoard(ctx, a.principalID, a.businessID, boardB.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant GetBoard err = %v, want ErrNotFound", err)
	}
	// Principal A asking for B's board under B's business (which A can't see) → not found (no oracle).
	if _, err := svc.GetBoard(ctx, a.principalID, b.businessID, boardB.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant GetBoard (B biz) err = %v, want ErrNotFound", err)
	}
}

// TestConvertToTicketIdempotent converts a post to a ticket, verifies the link, and asserts a
// second convert returns the same ticket id (idempotent) and that a ticket row exists.
func TestConvertToTicketIdempotent(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	pid, biz := seed.principalID, seed.businessID
	svc := &feedback.Service{DB: tdb.App}

	board, _ := svc.CreateBoard(ctx, pid, biz, feedback.BoardInput{Name: "B"})
	post, _ := svc.CreatePost(ctx, pid, biz, board.ID, feedback.PostInput{Title: "Ship it"})

	tid, err := svc.ConvertToTicket(ctx, pid, biz, post.ID)
	if err != nil || tid == uuid.Nil {
		t.Fatalf("ConvertToTicket: id=%s err=%v", tid, err)
	}
	// The post is now linked.
	got, _ := svc.GetPost(ctx, pid, biz, post.ID)
	if got.TicketID == nil || *got.TicketID != tid {
		t.Fatalf("post.ticket_id = %v, want %s", got.TicketID, tid)
	}
	// Idempotent.
	tid2, err := svc.ConvertToTicket(ctx, pid, biz, post.ID)
	if err != nil || tid2 != tid {
		t.Fatalf("second convert: id=%s err=%v, want %s", tid2, err, tid)
	}
	// A ticket row exists with a feedback reply_token.
	var subject, replyToken string
	if err := tdb.Super.QueryRow(ctx,
		"SELECT subject, reply_token FROM ticket WHERE id=$1", tid).Scan(&subject, &replyToken); err != nil {
		t.Fatalf("ticket row: %v", err)
	}
	if subject != "Ship it" || replyToken != "fb:"+post.ID.String() {
		t.Fatalf("ticket subject=%q reply_token=%q", subject, replyToken)
	}
}

// TestPublicIngressAndOracle drives the principal-less SDK ingress over HTTP: submit + list +
// vote via a publishable key, and the uniform-401 oracle boundary for unknown / revoked keys
// and private boards.
func TestPublicIngressAndOracle(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	pid, biz := seed.principalID, seed.businessID
	svc := &feedback.Service{DB: tdb.App}

	// A public board + a private board, each with a key.
	pubBoard, _ := svc.CreateBoard(ctx, pid, biz, feedback.BoardInput{Name: "Public", IsPublic: true})
	privBoard, _ := svc.CreateBoard(ctx, pid, biz, feedback.BoardInput{Name: "Private"})
	pubKey, err := svc.CreateIngestKey(ctx, pid, biz, pubBoard.ID, nil)
	if err != nil {
		t.Fatalf("CreateIngestKey public: %v", err)
	}
	privKey, _ := svc.CreateIngestKey(ctx, pid, biz, privBoard.ID, nil)

	h := feedback.NewPublicHandler(tdb.App, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	r := chi.NewRouter()
	h.PublicRoutes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	post := func(t *testing.T, path string, body any) (int, map[string]any) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		resp, err := http.Post(srv.URL+path, "application/json", &buf)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Submit to the public board with the valid key → 201.
	code, out := post(t, "/feedback/public/"+pubKey.PublishableKey+"/posts",
		map[string]any{"title": "SDK feature", "author_identity": "device-1"})
	if code != http.StatusCreated {
		t.Fatalf("public submit code = %d, want 201 (%v)", code, out)
	}
	postID, _ := out["id"].(string)
	if postID == "" {
		t.Fatalf("public submit: no id in %v", out)
	}

	// Vote twice with the same identity → second is a no-op, count stays 1.
	code, out = post(t, "/feedback/public/"+pubKey.PublishableKey+"/posts/"+postID+"/votes",
		map[string]any{"voter_identity": "voter-A"})
	if code != http.StatusOK || out["voted"] != true || out["vote_count"].(float64) != 1 {
		t.Fatalf("first public vote: code=%d out=%v", code, out)
	}
	code, out = post(t, "/feedback/public/"+pubKey.PublishableKey+"/posts/"+postID+"/votes",
		map[string]any{"voter_identity": "voter-A"})
	if code != http.StatusOK || out["voted"] != false || out["vote_count"].(float64) != 1 {
		t.Fatalf("replay public vote: code=%d out=%v (want voted=false count=1)", code, out)
	}

	// List public posts → the submitted post is present.
	resp, err := http.Get(srv.URL + "/feedback/public/" + pubKey.PublishableKey + "/posts")
	if err != nil {
		t.Fatalf("public list: %v", err)
	}
	var listOut struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listOut)
	resp.Body.Close()
	if len(listOut.Items) != 1 || listOut.Items[0]["title"] != "SDK feature" {
		t.Fatalf("public list = %+v", listOut.Items)
	}

	// Oracle boundary: unknown key, private-board key, and a revoked key all → uniform 401.
	if code, _ := post(t, "/feedback/public/fbk_unknownkey/posts", map[string]any{"title": "x"}); code != http.StatusUnauthorized {
		t.Fatalf("unknown-key submit code = %d, want 401", code)
	}
	if code, _ := post(t, "/feedback/public/"+privKey.PublishableKey+"/posts", map[string]any{"title": "x"}); code != http.StatusUnauthorized {
		t.Fatalf("private-board-key submit code = %d, want 401", code)
	}
	if _, err := svc.RevokeIngestKey(ctx, pid, biz, pubKey.ID); err != nil {
		t.Fatalf("RevokeIngestKey: %v", err)
	}
	if code, _ := post(t, "/feedback/public/"+pubKey.PublishableKey+"/posts", map[string]any{"title": "x"}); code != http.StatusUnauthorized {
		t.Fatalf("revoked-key submit code = %d, want 401", code)
	}
}

// --- verified-identity tier (saz.5) helpers -----------------------------------------------

// newTestSealer builds a deterministic 32-byte-key Sealer for tests. Different fill bytes give
// different (incompatible) keys, used to simulate a wrong-key unseal failure.
func newTestSealer(t *testing.T, fill byte) *crypto.Sealer {
	t.Helper()
	key := bytes.Repeat([]byte{fill}, 32)
	s, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	return s
}

// startPublicServer mounts h's public routes on a fresh httptest server, closed automatically
// at the end of the (sub)test.
func startPublicServer(t *testing.T, h *feedback.PublicHandler) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	h.PublicRoutes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// discardLogger is the logger every test PublicHandler uses (assertions read HTTP responses,
// not log output).
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// marshalBody JSON-encodes v once so the exact same bytes are both signed and sent — a test that
// re-marshaled independently for signing vs sending could pass/fail on map key ordering rather
// than on the behavior under test.
func marshalBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return b
}

// feedbackSignHeader computes an X-Feedback-Signature header value exactly as a real SDK would:
// v1 = hex(HMAC_SHA256(secret, "<t>.<METHOD>.<target>.<body>")), where <target> is the full
// request-target (path + "?" + raw query string) for the request being signed — exactly what the
// server will see via r.URL.RequestURI(). Callers signing a request that carries a query string
// (e.g. the signed GET list with ?voter_identity=...) MUST pass that query as part of target, or
// the server's signature check (which covers the query) will reject it. Deliberately independent
// of internal/feedback/signature.go's (unexported) feedbackSigningString — this is an external
// black-box check of the wire contract, not a call into the implementation under test.
func feedbackSignHeader(secret string, ts int64, method, target string, body []byte) string {
	head := fmt.Sprintf("%d.%s.%s.", ts, method, target)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(head))
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// doPublicRequest issues one HTTP request against a public-ingress test server and decodes the
// JSON response body into a map (best-effort; a non-JSON body decodes to a nil map).
func doPublicRequest(t *testing.T, method, rawURL string, body []byte, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, reader)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, rawURL, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// viewerVotedFor finds postID in a decoded /posts list's "items" array and returns its
// viewer_voted flag (false if the post is absent from the page).
func viewerVotedFor(items []any, postID string) bool {
	for _, raw := range items {
		it, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if it["id"] == postID {
			vv, _ := it["viewer_voted"].(bool)
			return vv
		}
	}
	return false
}

// TestFeedbackPublicSignatureVerification drives real HTTP round-trips through PublicHandler
// against real Postgres with an actual HMAC-signed request, asserting the full §3 signature
// contract: a valid signature verifies, a tampered body / expired timestamp / malformed header
// all answer 401, and a MAC computed for one post's vote path cannot be replayed against a
// different post's vote path (method+path binding).
func TestFeedbackPublicSignatureVerification(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	sealer := newTestSealer(t, 0xAA)
	svc := &feedback.Service{DB: tdb.App, Sealer: sealer}

	board, err := svc.CreateBoard(ctx, seed.principalID, seed.businessID, feedback.BoardInput{Name: "Signed", IsPublic: true})
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	key, err := svc.CreateIngestKey(ctx, seed.principalID, seed.businessID, board.ID, nil)
	if err != nil {
		t.Fatalf("CreateIngestKey: %v", err)
	}
	if key.Secret == "" {
		t.Fatalf("CreateIngestKey: expected a plaintext secret with a sealer configured")
	}

	h := feedback.NewPublicHandler(tdb.App, discardLogger(), sealer)
	srv := startPublicServer(t, h)
	submitPath := "/feedback/public/" + key.PublishableKey + "/posts"

	t.Run("valid signature -> identity_verified=true", func(t *testing.T) {
		body := marshalBody(t, map[string]any{"title": "Signed post", "idempotency_key": "valid-1"})
		hdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodPost, submitPath, body)
		code, out := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, body, map[string]string{"X-Feedback-Signature": hdr})
		if code != http.StatusCreated || out["identity_verified"] != true {
			t.Fatalf("code=%d out=%v, want 201 identity_verified=true", code, out)
		}
	})

	t.Run("tampered body -> 401", func(t *testing.T) {
		signedBody := marshalBody(t, map[string]any{"title": "Original", "idempotency_key": "tamper-1"})
		hdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodPost, submitPath, signedBody)
		sentBody := marshalBody(t, map[string]any{"title": "TAMPERED", "idempotency_key": "tamper-1"})
		code, _ := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, sentBody, map[string]string{"X-Feedback-Signature": hdr})
		if code != http.StatusUnauthorized {
			t.Fatalf("tampered body code = %d, want 401", code)
		}
	})

	t.Run("expired -> 401", func(t *testing.T) {
		body := marshalBody(t, map[string]any{"title": "Expired", "idempotency_key": "exp-1"})
		hdr := feedbackSignHeader(key.Secret, time.Now().Unix()-301, http.MethodPost, submitPath, body)
		code, _ := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, body, map[string]string{"X-Feedback-Signature": hdr})
		if code != http.StatusUnauthorized {
			t.Fatalf("expired signature code = %d, want 401", code)
		}
	})

	t.Run("malformed header -> 401", func(t *testing.T) {
		body := marshalBody(t, map[string]any{"title": "Malformed", "idempotency_key": "mal-1"})
		code, _ := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, body, map[string]string{"X-Feedback-Signature": "garbage"})
		if code != http.StatusUnauthorized {
			t.Fatalf("malformed signature code = %d, want 401", code)
		}
	})

	t.Run("cross-post replay -> 401 (path binding)", func(t *testing.T) {
		// Two posts, created anonymously (submit needs no signature).
		p1Code, p1Out := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, marshalBody(t, map[string]any{"title": "P1"}), nil)
		p2Code, p2Out := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, marshalBody(t, map[string]any{"title": "P2"}), nil)
		if p1Code != http.StatusCreated || p2Code != http.StatusCreated {
			t.Fatalf("seed posts: p1=%d p2=%d", p1Code, p2Code)
		}
		p1ID, _ := p1Out["id"].(string)
		p2ID, _ := p2Out["id"].(string)
		votePath1 := "/feedback/public/" + key.PublishableKey + "/posts/" + p1ID + "/votes"
		votePath2 := "/feedback/public/" + key.PublishableKey + "/posts/" + p2ID + "/votes"
		voteBody := marshalBody(t, map[string]any{"voter_identity": "binder"})
		// Sign for post1's vote path, replay against post2's vote path.
		hdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodPost, votePath1, voteBody)
		code, _ := doPublicRequest(t, http.MethodPost, srv.URL+votePath2, voteBody, map[string]string{"X-Feedback-Signature": hdr})
		if code != http.StatusUnauthorized {
			t.Fatalf("cross-post replay code = %d, want 401 (path binding)", code)
		}
	})
}

// TestFeedbackPublicFailClosedMatrix asserts the signature-verification matrix: two of the three
// ways a signature can be unverifiable fail closed (401), never silently falling back to
// anonymous — no Sealer configured on the handler, and the configured Sealer can't decrypt the
// stored secret (wrong master key). The third case must NOT fail closed: a signature over a key
// whose sealed_secret is NULL (verified tier was never enabled for that key) has nothing to
// verify against, so it degrades to anonymous and the submit still succeeds (201).
func TestFeedbackPublicFailClosedMatrix(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	sealerA := newTestSealer(t, 0x11)
	svcA := &feedback.Service{DB: tdb.App, Sealer: sealerA}

	board, err := svcA.CreateBoard(ctx, seed.principalID, seed.businessID, feedback.BoardInput{Name: "FailClosed", IsPublic: true})
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	keyA, err := svcA.CreateIngestKey(ctx, seed.principalID, seed.businessID, board.ID, nil)
	if err != nil {
		t.Fatalf("CreateIngestKey (sealer A): %v", err)
	}
	submitPath := "/feedback/public/" + keyA.PublishableKey + "/posts"

	t.Run("sealer-nil handler + valid signature -> 401", func(t *testing.T) {
		hNil := feedback.NewPublicHandler(tdb.App, discardLogger(), nil)
		srv := startPublicServer(t, hNil)
		body := marshalBody(t, map[string]any{"title": "x", "idempotency_key": "fc-nil-1"})
		hdr := feedbackSignHeader(keyA.Secret, time.Now().Unix(), http.MethodPost, submitPath, body)
		code, _ := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, body, map[string]string{"X-Feedback-Signature": hdr})
		if code != http.StatusUnauthorized {
			t.Fatalf("sealer-nil handler + signature code = %d, want 401", code)
		}
	})

	t.Run("unseal failure (seal A, open B) -> 401", func(t *testing.T) {
		sealerB := newTestSealer(t, 0x22)
		hB := feedback.NewPublicHandler(tdb.App, discardLogger(), sealerB)
		srv := startPublicServer(t, hB)
		body := marshalBody(t, map[string]any{"title": "x", "idempotency_key": "fc-unseal-1"})
		hdr := feedbackSignHeader(keyA.Secret, time.Now().Unix(), http.MethodPost, submitPath, body)
		code, _ := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, body, map[string]string{"X-Feedback-Signature": hdr})
		if code != http.StatusUnauthorized {
			t.Fatalf("unseal-failure code = %d, want 401", code)
		}
	})

	t.Run("NULL sealed_secret + signature -> anon 201 (not 401)", func(t *testing.T) {
		svcNoSealer := &feedback.Service{DB: tdb.App} // Sealer nil at CREATE time -> sealed_secret stays NULL
		board2, err := svcNoSealer.CreateBoard(ctx, seed.principalID, seed.businessID, feedback.BoardInput{Name: "NoSecret", IsPublic: true})
		if err != nil {
			t.Fatalf("CreateBoard (no sealer): %v", err)
		}
		key2, err := svcNoSealer.CreateIngestKey(ctx, seed.principalID, seed.businessID, board2.ID, nil)
		if err != nil {
			t.Fatalf("CreateIngestKey (no sealer): %v", err)
		}
		if key2.Secret != "" || key2.HasSecret {
			t.Fatalf("key2 = %+v, want no secret (sealed_secret stays NULL)", key2)
		}
		hSealed := feedback.NewPublicHandler(tdb.App, discardLogger(), sealerA) // handler DOES have a sealer configured
		srv := startPublicServer(t, hSealed)
		path2 := "/feedback/public/" + key2.PublishableKey + "/posts"
		body := marshalBody(t, map[string]any{"title": "anon fallback", "idempotency_key": "fc-null-1"})
		// A caller sends SOME signature; there is nothing stored to verify it against, so this
		// must fall back to anonymous rather than fail closed.
		hdr := feedbackSignHeader("attacker-guess", time.Now().Unix(), http.MethodPost, path2, body)
		code, out := doPublicRequest(t, http.MethodPost, srv.URL+path2, body, map[string]string{"X-Feedback-Signature": hdr})
		if code != http.StatusCreated || out["identity_verified"] != false {
			t.Fatalf("NULL-secret + signature code=%d out=%v, want 201 identity_verified=false (anon fallback)", code, out)
		}
	})
}

// TestFeedbackPublicExactlyOnceIdempotency asserts the DEFINER claim-first exactly-once
// contract: replaying (key, idempotency_key, body) dedupes to the same post id with
// deduped=true/HTTP 200, the same idempotency_key with a DIFFERENT body is a 409 conflict,
// different idempotency keys each create their own post, and an empty idempotency_key opts out
// of dedupe entirely.
func TestFeedbackPublicExactlyOnceIdempotency(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	svc := &feedback.Service{DB: tdb.App}
	board, err := svc.CreateBoard(ctx, seed.principalID, seed.businessID, feedback.BoardInput{Name: "Idem", IsPublic: true})
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	key, err := svc.CreateIngestKey(ctx, seed.principalID, seed.businessID, board.ID, nil)
	if err != nil {
		t.Fatalf("CreateIngestKey: %v", err)
	}
	h := feedback.NewPublicHandler(tdb.App, discardLogger(), nil)
	srv := startPublicServer(t, h)
	path := "/feedback/public/" + key.PublishableKey + "/posts"

	t.Run("same key+idem+body twice -> one post, second deduped=true HTTP 200 same id", func(t *testing.T) {
		body := marshalBody(t, map[string]any{"title": "dedupe me", "idempotency_key": "idem-1"})
		code1, out1 := doPublicRequest(t, http.MethodPost, srv.URL+path, body, nil)
		if code1 != http.StatusCreated {
			t.Fatalf("first submit code = %d, want 201", code1)
		}
		code2, out2 := doPublicRequest(t, http.MethodPost, srv.URL+path, body, nil)
		if code2 != http.StatusOK || out2["deduped"] != true || out2["id"] != out1["id"] {
			t.Fatalf("second submit code=%d out=%v, want 200 deduped=true id=%v", code2, out2, out1["id"])
		}
	})

	t.Run("same key+idem, different body -> 409", func(t *testing.T) {
		body1 := marshalBody(t, map[string]any{"title": "original", "idempotency_key": "idem-2"})
		code1, _ := doPublicRequest(t, http.MethodPost, srv.URL+path, body1, nil)
		if code1 != http.StatusCreated {
			t.Fatalf("first submit code = %d, want 201", code1)
		}
		body2 := marshalBody(t, map[string]any{"title": "different", "idempotency_key": "idem-2"})
		code2, _ := doPublicRequest(t, http.MethodPost, srv.URL+path, body2, nil)
		if code2 != http.StatusConflict {
			t.Fatalf("conflicting-body submit code = %d, want 409", code2)
		}
	})

	t.Run("different idem keys -> two posts", func(t *testing.T) {
		body1 := marshalBody(t, map[string]any{"title": "a", "idempotency_key": "idem-3a"})
		body2 := marshalBody(t, map[string]any{"title": "b", "idempotency_key": "idem-3b"})
		code1, out1 := doPublicRequest(t, http.MethodPost, srv.URL+path, body1, nil)
		code2, out2 := doPublicRequest(t, http.MethodPost, srv.URL+path, body2, nil)
		if code1 != http.StatusCreated || code2 != http.StatusCreated || out1["id"] == out2["id"] {
			t.Fatalf("distinct idem keys: code1=%d code2=%d id1=%v id2=%v", code1, code2, out1["id"], out2["id"])
		}
	})

	t.Run("empty idem -> no dedupe", func(t *testing.T) {
		body := marshalBody(t, map[string]any{"title": "no idem key"})
		code1, out1 := doPublicRequest(t, http.MethodPost, srv.URL+path, body, nil)
		code2, out2 := doPublicRequest(t, http.MethodPost, srv.URL+path, body, nil)
		if code1 != http.StatusCreated || code2 != http.StatusCreated || out1["id"] == out2["id"] {
			t.Fatalf("empty idem: code1=%d code2=%d id1=%v id2=%v, want two distinct 201s", code1, code2, out1["id"], out2["id"])
		}
	})
}

// TestFeedbackPublicCrossTierIdempotencySquat asserts an anon submit and a verified (signed)
// submit sharing the same raw idempotency_key on the same key do NOT collide: the idem_key is
// tier-namespaced (a:/v:) before hitting feedback_ingest_idempotency, so both requests create
// their own post — a verified submission is never silently swallowed by an anon idempotency
// squat on the same key value.
func TestFeedbackPublicCrossTierIdempotencySquat(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	sealer := newTestSealer(t, 0x33)
	svc := &feedback.Service{DB: tdb.App, Sealer: sealer}
	board, err := svc.CreateBoard(ctx, seed.principalID, seed.businessID, feedback.BoardInput{Name: "Squat", IsPublic: true})
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	key, err := svc.CreateIngestKey(ctx, seed.principalID, seed.businessID, board.ID, nil)
	if err != nil {
		t.Fatalf("CreateIngestKey: %v", err)
	}
	h := feedback.NewPublicHandler(tdb.App, discardLogger(), sealer)
	srv := startPublicServer(t, h)
	path := "/feedback/public/" + key.PublishableKey + "/posts"

	anonBody := marshalBody(t, map[string]any{"title": "anon squat", "idempotency_key": "SQUAT"})
	codeAnon, outAnon := doPublicRequest(t, http.MethodPost, srv.URL+path, anonBody, nil)
	if codeAnon != http.StatusCreated {
		t.Fatalf("anon submit code = %d, want 201", codeAnon)
	}

	verBody := marshalBody(t, map[string]any{"title": "verified squat", "idempotency_key": "SQUAT"})
	hdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodPost, path, verBody)
	codeVer, outVer := doPublicRequest(t, http.MethodPost, srv.URL+path, verBody, map[string]string{"X-Feedback-Signature": hdr})
	if codeVer != http.StatusCreated {
		t.Fatalf("verified submit code = %d, want 201 (must NOT be swallowed by the anon idem key)", codeVer)
	}
	if outAnon["id"] == outVer["id"] {
		t.Fatalf("anon and verified submits with the same idempotency_key collapsed into one post: %v", outAnon["id"])
	}
	if outAnon["deduped"] != false || outVer["deduped"] != false {
		t.Fatalf("cross-tier squat: outAnon=%v outVer=%v, want both deduped=false", outAnon, outVer)
	}
}

// TestFeedbackPublicTierIsolationAndViewerVoted asserts viewer_voted is scoped to the caller's
// own tier namespace: a signed GET sees viewer_voted=true only for the identity that actually
// cast the verified vote (false for a different identity), and an UNSIGNED list querying the
// same raw identity string does NOT see the verified vote (it resolves to the a: namespace,
// disjoint from the v: row).
func TestFeedbackPublicTierIsolationAndViewerVoted(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	sealer := newTestSealer(t, 0x44)
	svc := &feedback.Service{DB: tdb.App, Sealer: sealer}
	board, err := svc.CreateBoard(ctx, seed.principalID, seed.businessID, feedback.BoardInput{Name: "Tiers", IsPublic: true})
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	key, err := svc.CreateIngestKey(ctx, seed.principalID, seed.businessID, board.ID, nil)
	if err != nil {
		t.Fatalf("CreateIngestKey: %v", err)
	}
	h := feedback.NewPublicHandler(tdb.App, discardLogger(), sealer)
	srv := startPublicServer(t, h)
	submitPath := "/feedback/public/" + key.PublishableKey + "/posts"

	_, postOut := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, marshalBody(t, map[string]any{"title": "vote target"}), nil)
	postID, _ := postOut["id"].(string)
	if postID == "" {
		t.Fatalf("seed post: %v", postOut)
	}
	votePath := "/feedback/public/" + key.PublishableKey + "/posts/" + postID + "/votes"

	voteBody := marshalBody(t, map[string]any{"voter_identity": "alice"})
	voteHdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodPost, votePath, voteBody)
	voteCode, voteOut := doPublicRequest(t, http.MethodPost, srv.URL+votePath, voteBody, map[string]string{"X-Feedback-Signature": voteHdr})
	if voteCode != http.StatusOK || voteOut["voted"] != true || voteOut["identity_verified"] != true {
		t.Fatalf("verified vote (alice) code=%d out=%v", voteCode, voteOut)
	}

	listPath := "/feedback/public/" + key.PublishableKey + "/posts"

	t.Run("signed list, same identity -> viewer_voted=true", func(t *testing.T) {
		target := listPath + "?voter_identity=alice"
		hdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodGet, target, nil)
		code, out := doPublicRequest(t, http.MethodGet, srv.URL+target, nil, map[string]string{"X-Feedback-Signature": hdr})
		items, _ := out["items"].([]any)
		if code != http.StatusOK || !viewerVotedFor(items, postID) {
			t.Fatalf("signed list (alice) code=%d out=%v, want viewer_voted=true", code, out)
		}
	})

	t.Run("signed list, different identity -> viewer_voted=false", func(t *testing.T) {
		target := listPath + "?voter_identity=bob"
		hdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodGet, target, nil)
		code, out := doPublicRequest(t, http.MethodGet, srv.URL+target, nil, map[string]string{"X-Feedback-Signature": hdr})
		items, _ := out["items"].([]any)
		if code != http.StatusOK || viewerVotedFor(items, postID) {
			t.Fatalf("signed list (bob) code=%d out=%v, want viewer_voted=false", code, out)
		}
	})

	t.Run("signed list, MAC for one query replayed against a different query -> 401 (query binding)", func(t *testing.T) {
		signedTarget := listPath + "?voter_identity=alice"
		hdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodGet, signedTarget, nil)
		replayedTarget := listPath + "?voter_identity=bob"
		code, _ := doPublicRequest(t, http.MethodGet, srv.URL+replayedTarget, nil, map[string]string{"X-Feedback-Signature": hdr})
		if code != http.StatusUnauthorized {
			t.Fatalf("query-mismatch replay code = %d, want 401 (captured MAC for ?voter_identity=alice must not verify against ?voter_identity=bob)", code)
		}
	})

	t.Run("unsigned list, same raw id -> a:-namespaced, does NOT see the verified vote", func(t *testing.T) {
		code, out := doPublicRequest(t, http.MethodGet, srv.URL+listPath+"?voter_identity=alice", nil, nil)
		items, _ := out["items"].([]any)
		if code != http.StatusOK || viewerVotedFor(items, postID) {
			t.Fatalf("unsigned list (alice) code=%d out=%v, want viewer_voted=false (crossing into v: would be a tier-isolation regression)", code, out)
		}
	})
}

// TestFeedbackIdempotencyTableUnreachableByPrincipal asserts feedback_ingest_idempotency is
// unreachable through the authenticated (principal-scoped) query path: the table has RLS
// enabled with no policy and (per migration 0104) no grant to manyforge_app, so a SELECT run
// under db.WithPrincipal must either be denied outright or return zero rows — never a real row.
func TestFeedbackIdempotencyTableUnreachableByPrincipal(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	svc := &feedback.Service{DB: tdb.App}
	board, err := svc.CreateBoard(ctx, seed.principalID, seed.businessID, feedback.BoardInput{Name: "IdemRLS", IsPublic: true})
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	key, err := svc.CreateIngestKey(ctx, seed.principalID, seed.businessID, board.ID, nil)
	if err != nil {
		t.Fatalf("CreateIngestKey: %v", err)
	}
	h := feedback.NewPublicHandler(tdb.App, discardLogger(), nil)
	srv := startPublicServer(t, h)
	path := "/feedback/public/" + key.PublishableKey + "/posts"

	// Populate at least one idempotency row so a leak would be observable, not a vacuous pass.
	code, _ := doPublicRequest(t, http.MethodPost, srv.URL+path, marshalBody(t, map[string]any{"title": "seed", "idempotency_key": "rls-check"}), nil)
	if code != http.StatusCreated {
		t.Fatalf("seed submit code = %d, want 201", code)
	}
	var rowCount int
	if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM feedback_ingest_idempotency").Scan(&rowCount); err != nil {
		t.Fatalf("superuser count: %v", err)
	}
	if rowCount == 0 {
		t.Fatalf("expected at least one idempotency row after the seed submit, got 0")
	}

	err = tdb.App.WithPrincipal(ctx, seed.principalID, func(tx pgx.Tx) error {
		rows, qerr := tx.Query(ctx, "SELECT key_id FROM feedback_ingest_idempotency LIMIT 1")
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		if rows.Next() {
			t.Fatalf("feedback_ingest_idempotency leaked a row to a principal-scoped query — RLS lockdown regressed")
		}
		return rows.Err()
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("principal-scoped SELECT on feedback_ingest_idempotency: got err=%v, want permission-denied (42501) or zero rows", err)
		}
	}
}

// TestFeedbackVerifiedIdentityNamespaceInvariant pins the going-forward tier-namespace invariant
// migration 0104's backfill exists to establish: anon and verified identities occupy disjoint
// a:/v: prefixes — even an anon caller whose raw identity already starts with "v:" cannot land
// bare in the verified namespace — and an internal (principal-authored) vote (raw UUID, exactly
// the row shape the backfill's `NOT IN (SELECT id::text FROM principal)` clause excludes) is
// untouched and still cannot be double-voted through the authenticated Service.Vote path.
//
// NOTE: testdb.Start (used by every other test in this file) always runs every migration
// (including 0104) against a fresh, empty database before any test code executes, so it cannot
// seed genuinely pre-0104 rows — this test instead asserts the resulting a:/v: namespace
// invariant end-to-end through the public HTTP surface. TestFeedbackVerifiedIdentityBackfillMigration0104
// below exercises migration 0104's actual backfill UPDATE statements against real pre-0104 rows,
// via a version-capped test-local Postgres + migrator independent of testdb.Start.
func TestFeedbackVerifiedIdentityNamespaceInvariant(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	sealer := newTestSealer(t, 0x55)
	svc := &feedback.Service{DB: tdb.App, Sealer: sealer}

	pubBoard, err := svc.CreateBoard(ctx, seed.principalID, seed.businessID, feedback.BoardInput{Name: "Namespace", IsPublic: true})
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	key, err := svc.CreateIngestKey(ctx, seed.principalID, seed.businessID, pubBoard.ID, nil)
	if err != nil {
		t.Fatalf("CreateIngestKey: %v", err)
	}
	h := feedback.NewPublicHandler(tdb.App, discardLogger(), sealer)
	srv := startPublicServer(t, h)
	submitPath := "/feedback/public/" + key.PublishableKey + "/posts"
	_, postOut := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, marshalBody(t, map[string]any{"title": "namespace target"}), nil)
	postID, _ := postOut["id"].(string)
	if postID == "" {
		t.Fatalf("seed post: %v", postOut)
	}
	votePath := "/feedback/public/" + key.PublishableKey + "/posts/" + postID + "/votes"

	// Anon caller submits a raw identity that already looks like a verified one; the anon path
	// must still double-prefix it ("a:v:spoof"), never landing bare in the v: namespace.
	anonBody := marshalBody(t, map[string]any{"voter_identity": "v:spoof"})
	codeAnon, outAnon := doPublicRequest(t, http.MethodPost, srv.URL+votePath, anonBody, nil)
	if codeAnon != http.StatusOK || outAnon["voted"] != true {
		t.Fatalf("anon spoof vote code=%d out=%v", codeAnon, outAnon)
	}

	// A genuinely verified caller votes with the raw identity "spoof" -> stored "v:spoof", a
	// DISTINCT row from the anon "a:v:spoof" row above.
	verBody := marshalBody(t, map[string]any{"voter_identity": "spoof"})
	verHdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodPost, votePath, verBody)
	codeVer, outVer := doPublicRequest(t, http.MethodPost, srv.URL+votePath, verBody, map[string]string{"X-Feedback-Signature": verHdr})
	if codeVer != http.StatusOK || outVer["voted"] != true || outVer["identity_verified"] != true {
		t.Fatalf("verified spoof vote code=%d out=%v", codeVer, outVer)
	}
	if got, want := outVer["vote_count"], float64(2); got != want {
		t.Fatalf("vote_count = %v, want %v (two disjoint rows: a:v:spoof and v:spoof)", got, want)
	}

	rows, err := tdb.Super.Query(ctx, "SELECT voter_identity FROM feedback_vote WHERE post_id = $1 ORDER BY voter_identity", postID)
	if err != nil {
		t.Fatalf("query votes: %v", err)
	}
	var stored []string
	for rows.Next() {
		var vi string
		if err := rows.Scan(&vi); err != nil {
			t.Fatalf("scan: %v", err)
		}
		stored = append(stored, vi)
	}
	rows.Close()
	if len(stored) != 2 || stored[0] != "a:v:spoof" || stored[1] != "v:spoof" {
		t.Fatalf("stored voter identities = %v, want [a:v:spoof v:spoof]", stored)
	}

	// Internal (principal-authored) vote: raw UUID, untouched by any tier prefix, and
	// double-voting through the authenticated Service.Vote path is still a no-op.
	internalBoard, err := svc.CreateBoard(ctx, seed.principalID, seed.businessID, feedback.BoardInput{Name: "Internal"})
	if err != nil {
		t.Fatalf("CreateBoard internal: %v", err)
	}
	internalPost, err := svc.CreatePost(ctx, seed.principalID, seed.businessID, internalBoard.ID, feedback.PostInput{Title: "internal"})
	if err != nil {
		t.Fatalf("CreatePost internal: %v", err)
	}
	voted, count, err := svc.Vote(ctx, seed.principalID, seed.businessID, internalPost.ID)
	if err != nil || !voted || count != 1 {
		t.Fatalf("first internal vote: voted=%v count=%d err=%v", voted, count, err)
	}
	voted2, count2, err := svc.Vote(ctx, seed.principalID, seed.businessID, internalPost.ID)
	if err != nil || voted2 || count2 != 1 {
		t.Fatalf("replay internal vote: voted=%v count=%d err=%v, want voted=false count=1 (no double vote)", voted2, count2, err)
	}
	var internalIdentity string
	if err := tdb.Super.QueryRow(ctx, "SELECT voter_identity FROM feedback_vote WHERE post_id = $1", internalPost.ID).Scan(&internalIdentity); err != nil {
		t.Fatalf("query internal vote: %v", err)
	}
	if internalIdentity != seed.principalID.String() {
		t.Fatalf("internal vote voter_identity = %q, want raw principal UUID %q (untouched by a:/v: namespacing)", internalIdentity, seed.principalID.String())
	}
}

// --- authenticated-surface coverage (saz.5 gap-fill) --------------------------------------

// TestFeedbackCreateKeySecretWriteOnceOverHTTP exercises the create-only DTO split (handler.go's
// createIngestKeyResp vs ingestKeyResp) as a security boundary over real HTTP, not just the Go
// struct shape: CreateIngestKey's response is the ONLY place the plaintext fbs_ secret can ever
// appear on the wire. List and revoke are wired through ingestKeyResp, which has no Secret field,
// so this proves list/get/revoke are structurally unable to leak the plaintext — or the sealed
// blob persisted alongside it.
func TestFeedbackCreateKeySecretWriteOnceOverHTTP(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	sealer := newTestSealer(t, 0x88)
	svc := &feedback.Service{DB: tdb.App, Sealer: sealer}
	board, err := svc.CreateBoard(ctx, seed.principalID, seed.businessID, feedback.BoardInput{Name: "Secrets"})
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}

	h := feedback.NewHandler(svc)
	r := chi.NewRouter()
	// Stand in for the real auth middleware (mirrors httpx.WithPrincipal's doc comment: "for
	// handler tests that bypass the auth middleware").
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(httpx.WithPrincipal(req.Context(), seed.principalID)))
		})
	})
	h.ReadRoutes(r)
	h.WriteRoutes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	createPath := fmt.Sprintf("/businesses/%s/feedback/boards/%s/keys", seed.businessID, board.ID)
	createResp, err := http.Post(srv.URL+createPath, "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("POST create key: %v", err)
	}
	createRaw, _ := io.ReadAll(createResp.Body)
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create key code = %d, body=%s", createResp.StatusCode, createRaw)
	}
	if n := strings.Count(string(createRaw), "fbs_"); n != 1 {
		t.Fatalf("create response contains %d occurrences of the fbs_ secret prefix, want exactly 1: %s", n, createRaw)
	}
	var createOut map[string]any
	if err := json.Unmarshal(createRaw, &createOut); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	secret, _ := createOut["secret"].(string)
	if !strings.HasPrefix(secret, "fbs_") || createOut["has_secret"] != true {
		t.Fatalf("create response = %v, want a fbs_ secret and has_secret=true", createOut)
	}
	keyID, _ := createOut["id"].(string)
	if keyID == "" {
		t.Fatalf("create response missing id: %v", createOut)
	}

	// The sealed blob persisted in the DB must also never appear on the wire — assert against it
	// directly (not just the plaintext) so a future "helpfully echo the encrypted value" bug is
	// caught too.
	var sealedSecret string
	if err := tdb.Super.QueryRow(ctx, "SELECT sealed_secret FROM feedback_ingest_key WHERE id = $1", keyID).Scan(&sealedSecret); err != nil {
		t.Fatalf("read sealed_secret: %v", err)
	}
	if sealedSecret == "" {
		t.Fatalf("expected a non-empty sealed_secret with a sealer configured")
	}

	assertNoSecretLeak := func(t *testing.T, label string, raw []byte) {
		t.Helper()
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s response leaks the plaintext secret: %s", label, raw)
		}
		if strings.Contains(string(raw), "fbs_") {
			t.Fatalf("%s response contains the fbs_ secret prefix: %s", label, raw)
		}
		if strings.Contains(string(raw), sealedSecret) {
			t.Fatalf("%s response leaks the sealed secret blob: %s", label, raw)
		}
	}

	t.Run("list -> no secret, has_secret=true", func(t *testing.T) {
		listPath := fmt.Sprintf("/businesses/%s/feedback/boards/%s/keys", seed.businessID, board.ID)
		resp, err := http.Get(srv.URL + listPath)
		if err != nil {
			t.Fatalf("GET list keys: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list keys code = %d, body=%s", resp.StatusCode, raw)
		}
		assertNoSecretLeak(t, "list", raw)
		var out struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode list response: %v", err)
		}
		if len(out.Items) != 1 || out.Items[0]["has_secret"] != true {
			t.Fatalf("list items = %v, want 1 item with has_secret=true", out.Items)
		}
	})

	t.Run("revoke -> no secret, has_secret=true", func(t *testing.T) {
		revokePath := fmt.Sprintf("/businesses/%s/feedback/keys/%s/revoke", seed.businessID, keyID)
		resp, err := http.Post(srv.URL+revokePath, "application/json", nil)
		if err != nil {
			t.Fatalf("POST revoke key: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("revoke key code = %d, body=%s", resp.StatusCode, raw)
		}
		assertNoSecretLeak(t, "revoke", raw)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode revoke response: %v", err)
		}
		if out["has_secret"] != true || out["status"] != "revoked" {
			t.Fatalf("revoke response = %v, want has_secret=true status=revoked", out)
		}
	})
}

// TestFeedbackPublicAuthorFilterNamespaceIsolation asserts the read-path author filter is
// tier-namespaced exactly like the write path: a signed GET for a verified author's raw identity
// returns only that author's post (with identity_verified=true), while an UNSIGNED GET for the
// SAME raw identity string resolves into the disjoint a: namespace and returns nothing — the
// filter cannot be used to discover a verified author's posts by guessing their raw identity.
func TestFeedbackPublicAuthorFilterNamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	seed := seedFeedbackTenant(ctx, t, tdb)
	sealer := newTestSealer(t, 0x77)
	svc := &feedback.Service{DB: tdb.App, Sealer: sealer}
	board, err := svc.CreateBoard(ctx, seed.principalID, seed.businessID, feedback.BoardInput{Name: "Authors", IsPublic: true})
	if err != nil {
		t.Fatalf("CreateBoard: %v", err)
	}
	key, err := svc.CreateIngestKey(ctx, seed.principalID, seed.businessID, board.ID, nil)
	if err != nil {
		t.Fatalf("CreateIngestKey: %v", err)
	}
	h := feedback.NewPublicHandler(tdb.App, discardLogger(), sealer)
	srv := startPublicServer(t, h)
	submitPath := "/feedback/public/" + key.PublishableKey + "/posts"

	// An anon post from an unrelated author — proves the author filter isn't just "everything on
	// the board".
	anonBody := marshalBody(t, map[string]any{"title": "unrelated anon", "author_identity": "someone-else"})
	anonCode, _ := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, anonBody, nil)
	if anonCode != http.StatusCreated {
		t.Fatalf("anon submit code = %d, want 201", anonCode)
	}

	// A verified (signed) post from the target author.
	verBody := marshalBody(t, map[string]any{"title": "verified author post", "author_identity": "verified-author-1"})
	verHdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodPost, submitPath, verBody)
	verCode, verOut := doPublicRequest(t, http.MethodPost, srv.URL+submitPath, verBody, map[string]string{"X-Feedback-Signature": verHdr})
	if verCode != http.StatusCreated || verOut["identity_verified"] != true {
		t.Fatalf("verified submit code=%d out=%v", verCode, verOut)
	}
	verPostID, _ := verOut["id"].(string)

	listPath := "/feedback/public/" + key.PublishableKey + "/posts"

	t.Run("signed list, author=verified raw id -> only the verified post, identity_verified=true", func(t *testing.T) {
		target := listPath + "?author=verified-author-1"
		hdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodGet, target, nil)
		code, out := doPublicRequest(t, http.MethodGet, srv.URL+target, nil, map[string]string{"X-Feedback-Signature": hdr})
		items, _ := out["items"].([]any)
		if code != http.StatusOK || len(items) != 1 {
			t.Fatalf("signed author-filtered list code=%d items=%v, want exactly 1", code, items)
		}
		got, _ := items[0].(map[string]any)
		if got["id"] != verPostID || got["identity_verified"] != true {
			t.Fatalf("signed author-filtered list item = %v, want id=%s identity_verified=true", got, verPostID)
		}
	})

	t.Run("unsigned list, author=SAME raw id -> namespace isolation, returns nothing", func(t *testing.T) {
		target := listPath + "?author=verified-author-1"
		code, out := doPublicRequest(t, http.MethodGet, srv.URL+target, nil, nil)
		items, _ := out["items"].([]any)
		if code != http.StatusOK || len(items) != 0 {
			t.Fatalf("unsigned author-filtered list (same raw id) code=%d items=%v, want 0 (a:/v: namespace isolation)", code, items)
		}
	})
}

// --- migration 0104 backfill coverage (version-capped test-local migrator) -----------------

// startCappedPostgresContainer launches a throwaway Postgres container independent of
// testdb.Start (which always migrates straight to HEAD via m.Up(), leaving no legacy rows) and
// returns its host/port plus a *migrate.Migrate the caller can step to explicit versions with
// m.Migrate(n). This is the hook TestFeedbackVerifiedIdentityBackfillMigration0104 needs to seed
// genuinely pre-0104 rows and then observe migration 0104's backfill UPDATE statements run
// against them — something testdb.Start cannot do since it exposes no version-capping hook.
func startCappedPostgresContainer(ctx context.Context, t *testing.T) (host, port string, m *migrate.Migrate) {
	t.Helper()
	if os.Getenv("DOCKER_HOST") == "" {
		if out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output(); err == nil {
			if h := strings.TrimSpace(string(out)); h != "" {
				_ = os.Setenv("DOCKER_HOST", h)
			}
		}
	}
	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "" {
		_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	}

	ctr, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("manyforge"),
		postgres.WithUsername("manyforge"),
		postgres.WithPassword("devpassword"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		t.Fatalf("start capped-migration container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err = ctr.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	mapped, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	port = mapped.Port()

	// Colima forwards the mapped port to the host with a short lag after the container reports
	// ready; retry until the forward is live (mirrors testdb.connectWithRetry).
	adminDSN := fmt.Sprintf("postgres://manyforge:devpassword@%s:%s/manyforge?sslmode=disable", host, port)
	var pingErr error
	for range 30 {
		pool, perr := pgxpool.New(ctx, adminDSN)
		if perr == nil {
			if pingErr = pool.Ping(ctx); pingErr == nil {
				pool.Close()
				break
			}
			pool.Close()
		} else {
			pingErr = perr
		}
		time.Sleep(500 * time.Millisecond)
	}
	if pingErr != nil {
		t.Fatalf("connect to capped-migration container: %v", pingErr)
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("cannot locate migrations dir")
	}
	migDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	dbURL := fmt.Sprintf("pgx5://manyforge:devpassword@%s:%s/manyforge?sslmode=disable", host, port)
	m, err = migrate.New("file://"+migDir, dbURL)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return host, port, m
}

// TestFeedbackVerifiedIdentityBackfillMigration0104 runs migration 0104's actual backfill UPDATE
// statements end-to-end against real pre-0104 rows: migrate a fresh Postgres to version 103,
// hand-insert a raw PUBLIC feedback_vote (voter_identity="alice") and a raw INTERNAL
// (principal-authored) feedback_vote keyed on a real principal's UUID, migrate up to 104, then
// assert the public row was rewritten to "a:alice" while the principal-UUID row is byte-for-byte
// unchanged — and that the principal still cannot double-vote on that pre-existing row through
// the authenticated Service.Vote path after the backfill has run.
func TestFeedbackVerifiedIdentityBackfillMigration0104(t *testing.T) {
	ctx := context.Background()
	host, port, m := startCappedPostgresContainer(ctx, t)

	if err := m.Migrate(103); err != nil {
		t.Fatalf("migrate to 103: %v", err)
	}

	adminDSN := fmt.Sprintf("postgres://manyforge:devpassword@%s:%s/manyforge?sslmode=disable", host, port)
	super, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	defer super.Close()

	seed := seedFeedbackTenantPool(ctx, t, super)

	boardID := uuid.New()
	if _, err := super.Exec(ctx,
		`INSERT INTO feedback_board (id,business_id,tenant_root_id,slug,name,is_public,created_at,updated_at)
		 VALUES ($1,$2,$2,'legacy','Legacy',true,now(),now())`,
		boardID, seed.businessID); err != nil {
		t.Fatalf("seed pre-0104 board: %v", err)
	}

	// A raw PUBLIC post + vote, pre-0104 shape (no identity_verified column exists yet).
	publicPostID := uuid.New()
	if _, err := super.Exec(ctx,
		`INSERT INTO feedback_post (id,business_id,tenant_root_id,board_id,title,status,vote_count,author_kind,author_identity,created_at,updated_at)
		 VALUES ($1,$2,$2,$3,'Public post','open',1,'public','alice',now(),now())`,
		publicPostID, seed.businessID, boardID); err != nil {
		t.Fatalf("seed pre-0104 public post: %v", err)
	}
	if _, err := super.Exec(ctx,
		`INSERT INTO feedback_vote (id,business_id,tenant_root_id,post_id,voter_identity,created_at)
		 VALUES (gen_random_uuid(),$1,$1,$2,'alice',now())`,
		seed.businessID, publicPostID); err != nil {
		t.Fatalf("seed pre-0104 public vote: %v", err)
	}

	// A raw INTERNAL (principal-authored) post + a vote keyed on the principal's own raw UUID —
	// exactly the row shape migration 0104's `NOT IN (SELECT id::text FROM principal)` clause
	// excludes from the backfill.
	internalPostID := uuid.New()
	if _, err := super.Exec(ctx,
		`INSERT INTO feedback_post (id,business_id,tenant_root_id,board_id,title,status,vote_count,author_kind,author_principal_id,created_at,updated_at)
		 VALUES ($1,$2,$2,$3,'Internal post','open',1,'principal',$4,now(),now())`,
		internalPostID, seed.businessID, boardID, seed.principalID); err != nil {
		t.Fatalf("seed pre-0104 internal post: %v", err)
	}
	if _, err := super.Exec(ctx,
		`INSERT INTO feedback_vote (id,business_id,tenant_root_id,post_id,voter_identity,created_at)
		 VALUES (gen_random_uuid(),$1,$1,$2,$3,now())`,
		seed.businessID, internalPostID, seed.principalID.String()); err != nil {
		t.Fatalf("seed pre-0104 internal vote: %v", err)
	}

	if err := m.Migrate(104); err != nil {
		t.Fatalf("migrate to 104: %v", err)
	}

	var publicVoterIdentity string
	if err := super.QueryRow(ctx, "SELECT voter_identity FROM feedback_vote WHERE post_id = $1", publicPostID).Scan(&publicVoterIdentity); err != nil {
		t.Fatalf("read backfilled public vote: %v", err)
	}
	if publicVoterIdentity != "a:alice" {
		t.Fatalf("public vote voter_identity after backfill = %q, want %q", publicVoterIdentity, "a:alice")
	}

	var internalVoterIdentity string
	if err := super.QueryRow(ctx, "SELECT voter_identity FROM feedback_vote WHERE post_id = $1", internalPostID).Scan(&internalVoterIdentity); err != nil {
		t.Fatalf("read internal vote: %v", err)
	}
	if internalVoterIdentity != seed.principalID.String() {
		t.Fatalf("internal vote voter_identity after backfill = %q, want unchanged raw UUID %q", internalVoterIdentity, seed.principalID.String())
	}

	// Enable login for the app role (migrations create it NOLOGIN, mirrors testdb.Start) and
	// exercise the authenticated Service.Vote path: the principal must not be able to double-vote
	// on their pre-existing (backfill-untouched) row now that 0104 has run.
	if _, err := super.Exec(ctx, "ALTER ROLE manyforge_app LOGIN PASSWORD 'apppw'"); err != nil {
		t.Fatalf("enable app login: %v", err)
	}
	appDSN := fmt.Sprintf("postgres://manyforge_app:apppw@%s:%s/manyforge?sslmode=disable", host, port)
	app, err := appdb.Open(ctx, appDSN)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	defer app.Close()
	svc := &feedback.Service{DB: app}

	voted, count, err := svc.Vote(ctx, seed.principalID, seed.businessID, internalPostID)
	if err != nil || voted || count != 1 {
		t.Fatalf("post-backfill double-vote attempt: voted=%v count=%d err=%v, want voted=false count=1 (no double vote)", voted, count, err)
	}
}
