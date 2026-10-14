//go:build integration

package work_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
	"github.com/manyforge/manyforge/internal/work"
)

// tenant is a seeded tenant-root business with its owner principal.
type tenant struct {
	businessID uuid.UUID
	owner      uuid.UUID
}

func startDB(t *testing.T) (context.Context, *testdb.TestDB) {
	t.Helper()
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	return ctx, tdb
}

func superExec(ctx context.Context, t *testing.T, tdb *testdb.TestDB, sql string, args ...any) {
	t.Helper()
	if _, err := tdb.Super.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("seed exec: %v\nSQL: %s", err, sql)
	}
}

// addPrincipal seeds account → human principal with the given display name.
func addPrincipal(ctx context.Context, t *testing.T, tdb *testdb.TestDB, display string) uuid.UUID {
	t.Helper()
	acct, pid := uuid.New(), uuid.New()
	superExec(ctx, t, tdb, `INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at)
		VALUES ($1,$2,$3,'active',now(),now(),now())`, acct, "work-"+pid.String()+"@x.test", display)
	superExec(ctx, t, tdb, `INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, pid, acct)
	return pid
}

func grant(ctx context.Context, t *testing.T, tdb *testdb.TestDB, principalID, businessID uuid.UUID, roleKey string) {
	t.Helper()
	superExec(ctx, t, tdb, `INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at)
		SELECT $1,$2,$2,r.id,now() FROM role r WHERE r.tenant_root_id IS NULL AND r.key=$3`, principalID, businessID, roleKey)
}

func seedTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) tenant {
	t.Helper()
	tn := tenant{businessID: uuid.New(), owner: addPrincipal(ctx, t, tdb, "Owner Person")}
	superExec(ctx, t, tdb, `INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		VALUES ($1,NULL,$1,'WorkCo','active',now(),now())`, tn.businessID)
	superExec(ctx, t, tdb, `INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, tn.businessID)
	grant(ctx, t, tdb, tn.owner, tn.businessID, "owner")
	return tn
}

func sp(s string) *string { return &s }

func auditCount(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID uuid.UUID, action string) int {
	t.Helper()
	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM audit_entry WHERE business_id=$1 AND action=$2`, businessID, action).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

func TestWorkProjectTaskLifecycle(t *testing.T) {
	ctx, tdb := startDB(t)
	tn := seedTenant(ctx, t, tdb)
	svc := &work.Service{DB: tdb.App}

	proj, err := svc.CreateProject(ctx, tn.owner, tn.businessID, work.ProjectInput{Name: "ManyForge"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if proj.Slug != "manyforge" || proj.KeyPrefix != "manyforg" {
		t.Fatalf("derived slug/prefix = %q/%q", proj.Slug, proj.KeyPrefix)
	}
	if _, err := svc.CreateProject(ctx, tn.owner, tn.businessID, work.ProjectInput{Name: "Dup", Slug: "manyforge"}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("duplicate slug: err = %v, want ErrConflict", err)
	}
	mf, err := svc.CreateProject(ctx, tn.owner, tn.businessID, work.ProjectInput{Name: "Short", Slug: "short", KeyPrefix: "mf"})
	if err != nil {
		t.Fatalf("CreateProject explicit prefix: %v", err)
	}
	bySlug, err := svc.GetProject(ctx, tn.owner, tn.businessID, "short")
	if err != nil || bySlug.ID != mf.ID {
		t.Fatalf("GetProject by slug = %+v, %v", bySlug, err)
	}
	if _, err := svc.GetProject(ctx, tn.owner, tn.businessID, mf.ID.String()); err != nil {
		t.Fatalf("GetProject by id: %v", err)
	}
	if _, err := svc.GetProject(ctx, tn.owner, tn.businessID, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown slug: err = %v, want ErrNotFound", err)
	}

	task, err := svc.CreateTask(ctx, tn.owner, tn.businessID, "short", work.TaskFields{Title: sp("Rotate prod key"), Lane: sp("operator")})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if !strings.HasPrefix(task.Key, "mf-") || len(task.Key) != len("mf-")+6 {
		t.Fatalf("server key %q is not mf-<6>", task.Key)
	}
	if task.Status != "todo" || task.Priority != 2 || task.Type != "task" || task.Rev <= 0 || task.ClosedAt != nil {
		t.Fatalf("defaults: %+v", task)
	}
	if task.Labels == nil || task.BlockedBy == nil {
		t.Fatalf("labels/blocked_by must serialize as []")
	}
	if _, err := svc.CreateTask(ctx, tn.owner, tn.businessID, "short", work.TaskFields{}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("create without title: err = %v", err)
	}

	// done stamps closed_at and bumps rev; a note is appended with the caller's display name.
	done, err := svc.UpdateTask(ctx, tn.owner, tn.businessID, "short", task.Key, work.TaskFields{Status: sp("done")}, sp("rotated"))
	if err != nil {
		t.Fatalf("UpdateTask done: %v", err)
	}
	if done.ClosedAt == nil || done.Rev <= task.Rev || done.Lane != "operator" || done.Title != task.Title {
		t.Fatalf("after done: %+v", done)
	}
	if !strings.HasPrefix(done.Notes, "[") || !strings.HasSuffix(done.Notes, " Owner Person] rotated") {
		t.Fatalf("notes = %q", done.Notes)
	}
	// canceled keeps the original stamp; reopening clears it; a second note is newline-joined.
	canceled, err := svc.UpdateTask(ctx, tn.owner, tn.businessID, "short", task.Key, work.TaskFields{Status: sp("canceled")}, nil)
	if err != nil || canceled.ClosedAt == nil || !canceled.ClosedAt.Equal(*done.ClosedAt) {
		t.Fatalf("canceled must keep closed_at: %+v, %v", canceled, err)
	}
	reopened, err := svc.UpdateTask(ctx, tn.owner, tn.businessID, "short", task.Key, work.TaskFields{Status: sp("todo")}, sp("again"))
	if err != nil || reopened.ClosedAt != nil {
		t.Fatalf("reopen must clear closed_at: %+v, %v", reopened, err)
	}
	if lines := strings.Split(reopened.Notes, "\n"); len(lines) != 2 {
		t.Fatalf("notes should hold two entries, got %q", reopened.Notes)
	}
	if _, err := svc.UpdateTask(ctx, tn.owner, tn.businessID, "short", "mf-zzzzzz", work.TaskFields{Status: sp("done")}, nil); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown key: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.UpdateTask(ctx, tn.owner, tn.businessID, "short", task.Key, work.TaskFields{}, nil); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("empty patch: err = %v, want ErrValidation", err)
	}

	// The board hides tasks closed more than 14 days ago unless include_closed.
	old, err := svc.CreateTask(ctx, tn.owner, tn.businessID, "short", work.TaskFields{Title: sp("old"), Status: sp("done")})
	if err != nil || old.ClosedAt == nil {
		t.Fatalf("create done task: %+v, %v", old, err)
	}
	superExec(ctx, t, tdb, `UPDATE work_task SET closed_at = now() - interval '20 days' WHERE key=$1`, old.Key)
	backlog, err := svc.CreateTask(ctx, tn.owner, tn.businessID, "short", work.TaskFields{Title: sp("later"), Status: sp("backlog"), Priority: ptrInt(0)})
	if err != nil {
		t.Fatalf("create backlog: %v", err)
	}
	list, err := svc.ListTasks(ctx, tn.owner, tn.businessID, "short", false)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(list) != 2 || list[0].Key != backlog.Key {
		t.Fatalf("board = %v, want [backlog(p0), reopened] without the old closed task", keys(list))
	}
	all, err := svc.ListTasks(ctx, tn.owner, tn.businessID, "short", true)
	if err != nil || len(all) != 3 {
		t.Fatalf("include_closed: %v, %v", keys(all), err)
	}

	projects, err := svc.ListProjects(ctx, tn.owner, tn.businessID)
	if err != nil || len(projects) != 2 {
		t.Fatalf("ListProjects: %+v, %v", projects, err)
	}
	for _, p := range projects {
		if p.Slug == "short" && (p.Counts.Backlog != 1 || p.Counts.Todo != 1 || p.Counts.OperatorOpen != 1) {
			t.Errorf("short counts = %+v", p.Counts)
		}
	}
	sum, err := svc.Summary(ctx, tn.owner, tn.businessID)
	if err != nil || sum.OpenOperator != 1 || sum.OpenAgent != 1 || sum.InProgress != 0 {
		t.Fatalf("Summary = %+v, %v", sum, err)
	}

	if n := auditCount(ctx, t, tdb, tn.businessID, "work.project.created"); n != 2 {
		t.Errorf("work.project.created audit rows = %d, want 2", n)
	}
	if n := auditCount(ctx, t, tdb, tn.businessID, "work.task.created"); n != 3 {
		t.Errorf("work.task.created audit rows = %d, want 3", n)
	}
	if n := auditCount(ctx, t, tdb, tn.businessID, "work.task.updated"); n != 3 {
		t.Errorf("work.task.updated audit rows = %d, want 3", n)
	}
}

func TestWorkTenantIsolation(t *testing.T) {
	ctx, tdb := startDB(t)
	a := seedTenant(ctx, t, tdb)
	b := seedTenant(ctx, t, tdb)
	svc := &work.Service{DB: tdb.App}

	proj, err := svc.CreateProject(ctx, a.owner, a.businessID, work.ProjectInput{Name: "Secret", KeyPrefix: "sec"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	task, err := svc.CreateTask(ctx, a.owner, a.businessID, proj.Slug, work.TaskFields{Title: sp("hidden")})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// B's owner, addressing A's business or A's project through B's own business: always 404.
	for _, bid := range []uuid.UUID{a.businessID, b.businessID} {
		for _, ref := range []string{proj.ID.String(), proj.Slug} {
			if _, err := svc.GetProject(ctx, b.owner, bid, ref); !errors.Is(err, errs.ErrNotFound) {
				t.Errorf("GetProject(%s,%s): err = %v, want ErrNotFound", bid, ref, err)
			}
			if _, err := svc.ListTasks(ctx, b.owner, bid, ref, true); !errors.Is(err, errs.ErrNotFound) {
				t.Errorf("ListTasks: err = %v, want ErrNotFound", err)
			}
			if _, err := svc.UpdateTask(ctx, b.owner, bid, ref, task.Key, work.TaskFields{Title: sp("pwned")}, nil); !errors.Is(err, errs.ErrNotFound) {
				t.Errorf("UpdateTask: err = %v, want ErrNotFound", err)
			}
			if _, err := svc.CreateTask(ctx, b.owner, bid, ref, work.TaskFields{Title: sp("planted")}); !errors.Is(err, errs.ErrNotFound) {
				t.Errorf("CreateTask: err = %v, want ErrNotFound", err)
			}
			if _, err := svc.CreateToken(ctx, b.owner, bid, ref, "steal"); !errors.Is(err, errs.ErrNotFound) {
				t.Errorf("CreateToken: err = %v, want ErrNotFound", err)
			}
		}
	}
	if _, err := svc.ListProjects(ctx, b.owner, a.businessID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("ListProjects foreign business: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.Summary(ctx, b.owner, a.businessID); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("Summary foreign business: err = %v, want ErrNotFound", err)
	}
	var title string
	if err := tdb.Super.QueryRow(ctx, `SELECT title FROM work_task WHERE key=$1`, task.Key).Scan(&title); err != nil || title != "hidden" {
		t.Fatalf("A's task was modified: %q, %v", title, err)
	}
}

// syncClient drives POST /work/sync through the real handler.
type syncClient struct {
	t   *testing.T
	srv *httptest.Server
}

type syncResp struct {
	Rev     int64             `json:"rev"`
	Project map[string]string `json:"project"`
	Tasks   []work.Task       `json:"tasks"`
	Results []work.OpResult   `json:"results"`
	More    bool              `json:"more"`
}

func (c syncClient) do(token string, body any) (int, syncResp) {
	c.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		c.t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.srv.URL+"/work/sync", bytes.NewReader(raw))
	if err != nil {
		c.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("do: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	var out syncResp
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			c.t.Fatalf("decode: %v", err)
		}
	}
	return res.StatusCode, out
}

func TestWorkSyncEndToEnd(t *testing.T) {
	ctx, tdb := startDB(t)
	tn := seedTenant(ctx, t, tdb)
	member := addPrincipal(ctx, t, tdb, "Member Person")
	grant(ctx, t, tdb, member, tn.businessID, "member")
	svc := &work.Service{DB: tdb.App}

	proj, err := svc.CreateProject(ctx, tn.owner, tn.businessID, work.ProjectInput{Name: "ManyForge", Slug: "manyforge", KeyPrefix: "mf"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// The member (work.write via the member role) issues the token the agent uses.
	tok, err := svc.CreateToken(ctx, member, tn.businessID, proj.Slug, "laptop")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !strings.HasPrefix(tok.Token, "mfw_") || tok.Prefix != tok.Token[:12] {
		t.Fatalf("token = %q prefix %q", tok.Token, tok.Prefix)
	}
	var stored string
	if err := tdb.Super.QueryRow(ctx, `SELECT token_hash FROM work_token WHERE id=$1`, tok.ID).Scan(&stored); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if stored == tok.Token || len(stored) != 64 {
		t.Fatalf("token must be stored as a SHA-256 hex hash, got %q", stored)
	}

	r := chi.NewRouter()
	work.NewSyncHandler(svc, nil).Routes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	c := syncClient{t: t, srv: srv}

	ops := []map[string]any{
		{"op": "create", "key": "mf-aaaa01", "fields": map[string]any{"title": "Fix flake", "priority": 1, "labels": []string{"ci"}}},
		{"op": "create", "key": "mf-aaaa01", "fields": map[string]any{"title": "dup"}},
		{"op": "create", "key": "zz-aaaa02", "fields": map[string]any{"title": "foreign prefix"}},
		{"op": "create", "key": "mf-aaaa03", "fields": map[string]any{"title": "typo", "stauts": "done"}},
		{"op": "create", "key": "mf-aaaa04", "fields": map[string]any{"title": "Approve DNS", "lane": "operator"}},
		{"op": "update", "key": "mf-aaaa01", "fields": map[string]any{"status": "in_progress", "assignee": "claude"}},
		{"op": "update", "key": "mf-zzzz99", "fields": map[string]any{"status": "done"}},
		{"op": "note", "key": "mf-aaaa01", "text": "found the race", "author": "claude"},
		{"op": "note", "key": "mf-aaaa04", "text": "need a human"},
		{"op": "delete", "key": "mf-aaaa01"},
		{"op": "create", "key": "mf-aaaa05", "fields": map[string]any{
			"title": "imported", "status": "done", "created_at": "2026-01-02T03:04:05Z", "closed_at": "2026-01-03T00:00:00Z"}},
		{"op": "update", "key": "mf-aaaa05", "fields": map[string]any{"created_at": "2026-01-01T00:00:00Z"}},
		{"op": "create", "key": "mf-n6xh.5", "fields": map[string]any{"title": "beads child id"}},
	}
	code, out := c.do(tok.Token, map[string]any{"since": 0, "ops": ops})
	if code != http.StatusOK {
		t.Fatalf("sync status %d", code)
	}
	wantResults := []struct {
		ok  bool
		err string
	}{
		{true, ""}, {false, "key already exists"}, {false, "key must be mf-<id>, e.g. mf-k3x9q2 or mf-k3x9.1"},
		{false, `unknown field "stauts"`}, {true, ""}, {true, ""}, {false, "not found"}, {true, ""}, {true, ""},
		{false, "op must be create, update or note"},
		{true, ""}, {false, `unknown field "created_at"`}, {true, ""},
	}
	if len(out.Results) != len(wantResults) {
		t.Fatalf("results = %+v", out.Results)
	}
	for i, w := range wantResults {
		if got := out.Results[i]; got.OK != w.ok || got.Error != w.err || got.Key != ops[i]["key"] {
			t.Errorf("result %d = %+v, want ok=%v err=%q", i, got, w.ok, w.err)
		}
	}
	if out.Project["slug"] != "manyforge" || out.Project["key_prefix"] != "mf" {
		t.Errorf("project = %+v", out.Project)
	}
	if len(out.Tasks) != 4 {
		t.Fatalf("tasks = %v, want mf-aaaa01, mf-aaaa04, mf-aaaa05, mf-n6xh.5", keys(out.Tasks))
	}
	byKey := map[string]work.Task{}
	for i, task := range out.Tasks {
		byKey[task.Key] = task
		if i > 0 && task.Rev <= out.Tasks[i-1].Rev {
			t.Errorf("tasks not ordered by rev: %v", out.Tasks)
		}
	}
	fix := byKey["mf-aaaa01"]
	if fix.Status != "in_progress" || fix.Assignee != "claude" || fix.Priority != 1 || fix.Title != "Fix flake" ||
		!strings.HasSuffix(fix.Notes, " claude] found the race") {
		t.Errorf("mf-aaaa01 = %+v", fix)
	}
	imp := byKey["mf-aaaa05"]
	if !imp.CreatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) || imp.ClosedAt == nil ||
		!imp.ClosedAt.Equal(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("imported timestamps not preserved: %+v", imp)
	}
	if dns := byKey["mf-aaaa04"]; dns.Lane != "operator" || !strings.HasSuffix(dns.Notes, " agent] need a human") {
		t.Errorf("mf-aaaa04 = %+v", dns)
	}
	if out.Rev != out.Tasks[len(out.Tasks)-1].Rev || out.More {
		t.Errorf("rev = %d more = %v, want last task rev %d", out.Rev, out.More, out.Tasks[len(out.Tasks)-1].Rev)
	}

	// A beads-style dotted key is addressable through the web PATCH too.
	if _, err := svc.UpdateTask(ctx, tn.owner, tn.businessID, "manyforge", "mf-n6xh.5", work.TaskFields{Status: sp("blocked")}, nil); err != nil {
		t.Fatalf("web UpdateTask dotted key: %v", err)
	}
	code, out = c.do(tok.Token, map[string]any{"since": out.Rev})
	if code != http.StatusOK || len(out.Tasks) != 1 || out.Tasks[0].Status != "blocked" {
		t.Fatalf("pull after dotted-key edit: %d %+v", code, out)
	}

	// More than 1000 ops is rejected outright.
	tooMany := make([]map[string]any, 1001)
	for i := range tooMany {
		tooMany[i] = map[string]any{"op": "note", "key": "mf-aaaa01", "text": "x"}
	}
	if code, _ := c.do(tok.Token, map[string]any{"ops": tooMany}); code != http.StatusBadRequest {
		t.Errorf("1001 ops: %d, want 400", code)
	}

	// Cursor: nothing new → empty pull, rev unchanged. A web edit → exactly that task.
	cursor := out.Rev
	code, out = c.do(tok.Token, map[string]any{"since": cursor})
	if code != http.StatusOK || len(out.Tasks) != 0 || out.Rev != cursor || len(out.Results) != 0 {
		t.Fatalf("idle pull: %d %+v", code, out)
	}
	if _, err := svc.UpdateTask(ctx, tn.owner, tn.businessID, "manyforge", "mf-aaaa04", work.TaskFields{Status: sp("done")}, nil); err != nil {
		t.Fatalf("web UpdateTask: %v", err)
	}
	code, out = c.do(tok.Token, map[string]any{"since": cursor})
	if code != http.StatusOK || len(out.Tasks) != 1 || out.Tasks[0].Key != "mf-aaaa04" || out.Tasks[0].ClosedAt == nil {
		t.Fatalf("pull after web edit: %d %+v", code, out)
	}

	if n := auditCount(ctx, t, tdb, tn.businessID, "work.sync"); n != 1 {
		t.Errorf("work.sync audit rows = %d, want 1 (pure pulls are not audited)", n)
	}
	var lastUsed *time.Time
	if err := tdb.Super.QueryRow(ctx, `SELECT last_used_at FROM work_token WHERE id=$1`, tok.ID).Scan(&lastUsed); err != nil || lastUsed == nil {
		t.Errorf("last_used_at not stamped: %v, %v", lastUsed, err)
	}

	// Unknown token → 401.
	if code, _ := c.do("mfw_"+strings.Repeat("A", 43), map[string]any{}); code != http.StatusUnauthorized {
		t.Errorf("unknown token: %d, want 401", code)
	}
	// Issuer demoted to viewer (no work.write) → 401; restored → 200; removed → 401.
	superExec(ctx, t, tdb, `UPDATE membership SET role_id=(SELECT id FROM role WHERE tenant_root_id IS NULL AND key='viewer')
		WHERE principal_id=$1 AND business_id=$2`, member, tn.businessID)
	if code, _ := c.do(tok.Token, map[string]any{}); code != http.StatusUnauthorized {
		t.Errorf("issuer without work.write: %d, want 401", code)
	}
	superExec(ctx, t, tdb, `UPDATE membership SET role_id=(SELECT id FROM role WHERE tenant_root_id IS NULL AND key='member')
		WHERE principal_id=$1 AND business_id=$2`, member, tn.businessID)
	if code, _ := c.do(tok.Token, map[string]any{}); code != http.StatusOK {
		t.Fatalf("restored issuer: %d, want 200", code)
	}
	superExec(ctx, t, tdb, `UPDATE account SET status='deactivated' WHERE id=(SELECT account_id FROM principal WHERE id=$1)`, member)
	if code, _ := c.do(tok.Token, map[string]any{}); code != http.StatusUnauthorized {
		t.Errorf("deactivated issuer: %d, want 401", code)
	}
	superExec(ctx, t, tdb, `UPDATE account SET status='active' WHERE id=(SELECT account_id FROM principal WHERE id=$1)`, member)
	superExec(ctx, t, tdb, `DELETE FROM membership WHERE principal_id=$1 AND business_id=$2`, member, tn.businessID)
	if code, _ := c.do(tok.Token, map[string]any{}); code != http.StatusUnauthorized {
		t.Errorf("issuer removed from business: %d, want 401", code)
	}

	// Revoked token → 401 (owner-issued token, revoked through the API).
	ownerTok, err := svc.CreateToken(ctx, tn.owner, tn.businessID, proj.ID.String(), "ci")
	if err != nil {
		t.Fatalf("CreateToken owner: %v", err)
	}
	if code, _ := c.do(ownerTok.Token, map[string]any{}); code != http.StatusOK {
		t.Fatalf("owner token: %d, want 200", code)
	}
	if err := svc.RevokeToken(ctx, tn.owner, tn.businessID, proj.Slug, ownerTok.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if code, _ := c.do(ownerTok.Token, map[string]any{}); code != http.StatusUnauthorized {
		t.Errorf("revoked token: %d, want 401", code)
	}
	tokens, err := svc.ListTokens(ctx, tn.owner, tn.businessID, proj.Slug)
	if err != nil || len(tokens) != 2 {
		t.Fatalf("ListTokens: %+v, %v", tokens, err)
	}
	for _, tk := range tokens {
		if tk.ID == ownerTok.ID && tk.RevokedAt == nil {
			t.Errorf("revoked token lists revoked_at = nil")
		}
	}
	if n := auditCount(ctx, t, tdb, tn.businessID, "work.token.revoked"); n != 1 {
		t.Errorf("work.token.revoked audit rows = %d, want 1", n)
	}
	if n := auditCount(ctx, t, tdb, tn.businessID, "work.token.created"); n != 2 {
		t.Errorf("work.token.created audit rows = %d, want 2", n)
	}
}

func TestWorkSyncPerTokenRateLimit(t *testing.T) {
	ctx, tdb := startDB(t)
	tn := seedTenant(ctx, t, tdb)
	svc := &work.Service{DB: tdb.App}
	proj, err := svc.CreateProject(ctx, tn.owner, tn.businessID, work.ProjectInput{Name: "RL"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tok, err := svc.CreateToken(ctx, tn.owner, tn.businessID, proj.Slug, "x")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	r := chi.NewRouter()
	work.NewSyncHandler(svc, ratelimit.NewTokenBucket(0.001, 1)).Routes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	c := syncClient{t: t, srv: srv}
	if code, _ := c.do(tok.Token, map[string]any{}); code != http.StatusOK {
		t.Fatalf("first sync: %d", code)
	}
	if code, _ := c.do(tok.Token, map[string]any{}); code != http.StatusTooManyRequests {
		t.Fatalf("second sync: %d, want 429", code)
	}
}

func ptrInt(n int) *int { return &n }

func keys(ts []work.Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Key)
	}
	return out
}
