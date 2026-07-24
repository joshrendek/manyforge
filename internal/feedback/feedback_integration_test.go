//go:build integration

package feedback_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/feedback"
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

	h := feedback.NewPublicHandler(tdb.App, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
