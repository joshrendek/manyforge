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
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/feedback"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
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
	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}
	s := fbSeed{businessID: uuid.New(), principalID: uuid.New()}
	acctID := uuid.New()
	email := "fb-owner-" + s.businessID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
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
// v1 = hex(HMAC_SHA256(secret, "<t>.<METHOD>.<path>.<body>")). Deliberately independent of
// internal/feedback/signature.go's (unexported) feedbackSigningString — this is an
// external black-box check of the wire contract, not a call into the implementation under test.
func feedbackSignHeader(secret string, ts int64, method, path string, body []byte) string {
	head := fmt.Sprintf("%d.%s.%s.", ts, method, path)
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

// TestFeedbackPublicFailClosedMatrix asserts the three ways a signature can be unverifiable all
// fail closed (401), never silently falling back to anonymous: no Sealer configured on the
// handler, the configured Sealer can't decrypt the stored secret (wrong master key), and — the
// one case that must NOT fail closed — a signature over a key whose sealed_secret is NULL
// (verified tier was never enabled for that key) is treated as anonymous and still succeeds.
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
		hdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodGet, listPath, nil)
		code, out := doPublicRequest(t, http.MethodGet, srv.URL+listPath+"?voter_identity=alice", nil, map[string]string{"X-Feedback-Signature": hdr})
		items, _ := out["items"].([]any)
		if code != http.StatusOK || !viewerVotedFor(items, postID) {
			t.Fatalf("signed list (alice) code=%d out=%v, want viewer_voted=true", code, out)
		}
	})

	t.Run("signed list, different identity -> viewer_voted=false", func(t *testing.T) {
		hdr := feedbackSignHeader(key.Secret, time.Now().Unix(), http.MethodGet, listPath, nil)
		code, out := doPublicRequest(t, http.MethodGet, srv.URL+listPath+"?voter_identity=bob", nil, map[string]string{"X-Feedback-Signature": hdr})
		items, _ := out["items"].([]any)
		if code != http.StatusOK || viewerVotedFor(items, postID) {
			t.Fatalf("signed list (bob) code=%d out=%v, want viewer_voted=false", code, out)
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
// LIMITATION: testdb.Start always runs every migration (including 0104) against a fresh,
// empty database before any test code executes, so there is no way in this harness to seed
// genuinely pre-0104 rows and observe the backfill's UPDATE statements execute against them.
// This test asserts the invariant instead of the migration step itself; see task-10-report.md.
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
