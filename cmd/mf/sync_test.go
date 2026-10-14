package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer implements POST /api/v1/work/sync. By default it accepts every op
// and echoes created/updated tasks back with a server-side title tweak, so
// tests can tell the server snapshot from the local copy.
type fakeServer struct {
	*httptest.Server
	mu      sync.Mutex
	reqs    []syncRequest
	auth    []string
	status  int
	rev     int64
	failKey string // ops on this key are rejected with "boom"
	extra   []Task // returned on every pull
	pages   int    // answer more=true this many times
	queue   []int  // per-request statuses, consumed before status
	bodies  []int  // request body sizes
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{status: http.StatusOK, rev: 41}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/work/sync" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.bodies = append(f.bodies, len(body))
		var req syncRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode: %v", err)
		}
		f.reqs = append(f.reqs, req)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		status := f.status
		if len(f.queue) > 0 {
			status, f.queue = f.queue[0], f.queue[1:]
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"ERR","message":"nope"}`))
			return
		}
		f.rev++
		resp := syncResponse{Rev: f.rev, Project: &Project{Slug: "mf", Name: "ManyForge", KeyPrefix: "mf"}, Tasks: f.extra, More: f.pages > 0}
		f.pages--
		for _, op := range req.Ops {
			if op.Key == f.failKey {
				msg := "boom"
				if op.Op == "create" {
					msg = "conflict: key already exists"
				}
				resp.Results = append(resp.Results, opResult{Key: op.Key, Error: msg})
				continue
			}
			resp.Results = append(resp.Results, opResult{Key: op.Key, OK: true})
			if op.Op == "create" {
				resp.Tasks = append(resp.Tasks, Task{Key: op.Key, Title: "server: " + *op.Fields.Title,
					Lane: "agent", Status: "todo", Priority: 2, Type: "task", Labels: []string{}, BlockedBy: []string{}, Rev: f.rev})
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) requests() []syncRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]syncRequest(nil), f.reqs...)
}

func newSyncedEnv(t *testing.T) (*testEnv, *fakeServer) {
	srv := newFakeServer(t)
	e := newTestEnv(t)
	e.mustRun("init", "--server", srv.URL, "--project", "mf", "--prefix", "mf", "--no-docs")
	e.env["MF_TOKEN"] = "mfw_test"
	e.env["MF_SERVER"] = srv.URL
	return e, srv
}

func TestSyncRoundTrip(t *testing.T) {
	e, srv := newSyncedEnv(t)
	srv.extra = []Task{{Key: "mf-remote", Title: "made in the web UI", Lane: "operator", Status: "todo", Rev: 7}}

	key := e.mustRun("create", "Local title", "-p", "1")
	reqs := srv.requests()
	if len(reqs) != 1 || reqs[0].Since != 0 || len(reqs[0].Ops) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	op := reqs[0].Ops[0]
	if op.Op != "create" || op.Key != key || *op.Fields.Title != "Local title" || *op.Fields.Priority != 1 || *op.Fields.Lane != "agent" {
		t.Fatalf("op = %+v / %+v", op, op.Fields)
	}
	if srv.auth[0] != "Bearer mfw_test" {
		t.Fatalf("auth = %q", srv.auth[0])
	}
	st := e.state()
	if len(st.Outbox) != 0 || st.Cursor != 42 {
		t.Fatalf("outbox=%d cursor=%d", len(st.Outbox), st.Cursor)
	}
	if got := st.Tasks[key]; got.Title != "server: Local title" || got.Rev != 42 || got.Priority != 2 {
		t.Fatalf("server snapshot should win: %+v", got)
	}
	if st.Tasks["mf-remote"] == nil || st.Project == nil || st.Project.Name != "ManyForge" {
		t.Fatalf("pull missing remote task / project: %+v", st)
	}

	// The next push carries the advanced cursor and only the new op.
	e.mustRun("note", key, "progress")
	reqs = srv.requests()
	if last := reqs[len(reqs)-1]; last.Since != 42 || len(last.Ops) != 1 || last.Ops[0].Op != "note" ||
		last.Ops[0].Text != "progress" || last.Ops[0].Author != "tester" {
		t.Fatalf("second request = %+v", last)
	}
}

func TestSyncPerOpErrorsWarnAndDrop(t *testing.T) {
	e, srv := newSyncedEnv(t)
	e.env["MF_OFFLINE"] = "1"
	key := e.mustRun("create", "will collide")
	e.mustRun("update", key, "--status", "blocked")
	delete(e.env, "MF_OFFLINE")
	srv.failKey = key

	if out := e.mustRun("sync"); !strings.Contains(out, "pushed 2 op(s) (2 failed)") {
		t.Fatalf("sync stdout = %q", out)
	}
	stderr := e.err.String()
	if !strings.Contains(stderr, "mf: sync: "+key+": key already exists on the server") {
		t.Fatalf("missing create-conflict warning: %q", stderr)
	}
	if !strings.Contains(stderr, "mf: sync: "+key+": boom") {
		t.Fatalf("missing per-op warning: %q", stderr)
	}
	if st := e.state(); len(st.Outbox) != 0 {
		t.Fatalf("rejected ops must still be dropped: %+v", st.Outbox)
	}
}

func TestSync401KeepsOutbox(t *testing.T) {
	e, srv := newSyncedEnv(t)
	srv.status = http.StatusUnauthorized

	key := e.mustRun("create", "offline-ish") // mutation still succeeds locally
	if !strings.Contains(e.err.String(), "token rejected") || !strings.Contains(e.err.String(), "mf login") {
		t.Fatalf("stderr = %q", e.err.String())
	}
	if st := e.state(); len(st.Outbox) != 1 || st.Outbox[0].Key != key || st.Cursor != 0 {
		t.Fatalf("outbox must be kept on 401: %+v", st)
	}
	if code := e.run("sync"); code != 1 || !strings.Contains(e.err.String(), "token rejected") {
		t.Fatalf("explicit sync: exit %d stderr %q", code, e.err.String())
	}
}

func TestOfflineSkipsSync(t *testing.T) {
	e, srv := newSyncedEnv(t)
	e.env["MF_OFFLINE"] = "1"
	e.mustRun("create", "one")
	delete(e.env, "MF_OFFLINE")
	e.mustRun("--no-sync", "create", "two")
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("offline mutations hit the server %d times", n)
	}
	if st := e.state(); len(st.Outbox) != 2 {
		t.Fatalf("outbox = %d, want 2", len(st.Outbox))
	}
	e.mustRun("sync")
	if reqs := srv.requests(); len(reqs) != 1 || len(reqs[0].Ops) != 2 {
		t.Fatalf("explicit sync should push both queued ops: %+v", reqs)
	}
	if st := e.state(); len(st.Outbox) != 0 {
		t.Fatal("outbox not cleared")
	}
}

func TestNoTokenWarnsAndStaysLocal(t *testing.T) {
	e, srv := newSyncedEnv(t)
	delete(e.env, "MF_TOKEN")
	e.mustRun("create", "x")
	if !strings.Contains(e.err.String(), "not synced: no token") || len(srv.requests()) != 0 {
		t.Fatalf("stderr = %q", e.err.String())
	}
	if code := e.run("sync"); code != 1 {
		t.Fatalf("sync without token: exit %d", code)
	}
}

func TestSyncURL(t *testing.T) {
	for server, ok := range map[string]bool{
		"https://forge.example":     true,
		"https://forge.example/":    true,
		"http://127.0.0.1:8081":     true,
		"http://localhost:8081":     true,
		"http://[::1]:8081":         true,
		"http://forge.example":      false, // token in clear text
		"ftp://forge.example":       false,
		"forge.example":             false,
		"http://10.0.0.5:8080/base": false,
	} {
		u, err := syncURL(server)
		if (err == nil) != ok {
			t.Errorf("syncURL(%q) = %q, %v", server, u, err)
		}
		if ok && !strings.HasSuffix(u, "/api/v1/work/sync") || strings.Contains(u, "//api") {
			t.Errorf("syncURL(%q) = %q", server, u)
		}
	}
}

func TestPrimeOutsideRepoPrintsNothing(t *testing.T) {
	e := newTestEnv(t)
	if code := e.run("prime"); code != 0 || e.out.Len() != 0 || e.err.Len() != 0 {
		t.Fatalf("exit %d stdout %q stderr %q", code, e.out.String(), e.err.String())
	}
}

func TestPrimeNeverFails(t *testing.T) {
	e, srv := newSyncedEnv(t)
	e.env["MF_OFFLINE"] = "1"
	ready := e.mustRun("create", "ready work", "-p", "0")
	op := e.mustRun("create", "--operator", "approve DNS")
	delete(e.env, "MF_OFFLINE")
	srv.status = http.StatusInternalServerError

	start := time.Now()
	if code := e.run("prime"); code != 0 {
		t.Fatalf("prime exit %d", code)
	}
	out := e.out.String()
	for _, want := range []string{"mf create --operator", "## Ready (1)", ready + " [P0] [todo] [agent] ready work",
		"## Operator inbox", op + " [P2] [todo] [operator] approve DNS", "2 local change(s) not yet synced"} {
		if !strings.Contains(out, want) {
			t.Errorf("prime output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(e.err.String(), "sync skipped") {
		t.Errorf("stderr = %q", e.err.String())
	}

	srv.Close() // unreachable server: still exit 0, within the short timeout
	if code := e.run("prime"); code != 0 || !strings.Contains(e.out.String(), "## Ready") {
		t.Fatalf("prime with server down: exit %d", code)
	}
	if time.Since(start) > 2*primeTimeout {
		t.Fatal("prime took too long")
	}
}

// queueOps appends n update ops straight into the outbox (faster than n CLI runs).
func queueOps(t *testing.T, e *testEnv, n int, desc string) {
	t.Helper()
	r, err := e.repo()
	if err != nil {
		t.Fatal(err)
	}
	err = r.withState(func(st *State) error {
		for i := range n {
			st.Outbox = append(st.Outbox, Op{Op: "update", Key: fmt.Sprintf("mf-op%04d", i), Fields: &patch{Description: ptr(desc)}})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSyncChunksByCount(t *testing.T) {
	e, srv := newSyncedEnv(t)
	queueOps(t, e, 2500, "")
	if out := e.mustRun("sync"); !strings.Contains(out, "pushed 2500 op(s)") || !strings.Contains(out, "in 3 request(s)") {
		t.Fatalf("sync = %q", out)
	}
	var sizes []int
	for _, r := range srv.requests() {
		sizes = append(sizes, len(r.Ops))
	}
	if fmt.Sprint(sizes) != "[1000 1000 500]" {
		t.Fatalf("batch sizes = %v", sizes)
	}
	if reqs := srv.requests(); reqs[1].Since != 42 || reqs[2].Since != 43 {
		t.Fatalf("each batch must carry the advanced cursor: %d %d", reqs[1].Since, reqs[2].Since)
	}
}

func TestSyncChunksByBytes(t *testing.T) {
	e, srv := newSyncedEnv(t)
	queueOps(t, e, 12, strings.Repeat("x", 100<<10)) // ~1.2 MiB of ops in total
	e.mustRun("sync")
	if n := len(srv.requests()); n < 3 {
		t.Fatalf("expected the ops split across requests, got %d", n)
	}
	for _, size := range srv.bodies {
		if size > 600<<10 {
			t.Fatalf("request body %d bytes; must stay well under 1 MiB", size)
		}
	}
	if st := e.state(); len(st.Outbox) != 0 {
		t.Fatal("outbox not drained")
	}
}

func TestSyncFollowsMorePages(t *testing.T) {
	e, srv := newSyncedEnv(t)
	srv.pages = 2
	e.mustRun("sync")
	reqs := srv.requests()
	if len(reqs) != 3 || reqs[1].Since != 42 || reqs[2].Since != 43 {
		t.Fatalf("want 3 pulls with advancing since, got %+v", reqs)
	}
	srv.pages = 1000 // a server that never stops is capped
	e.mustRun("sync")
	if n := len(srv.requests()) - 3; n != maxPullRounds {
		t.Fatalf("pull rounds = %d, want cap %d", n, maxPullRounds)
	}
}

func TestSyncRateLimitedKeepsUnsentOps(t *testing.T) {
	e, srv := newSyncedEnv(t)
	queueOps(t, e, 2500, "")
	srv.queue = []int{http.StatusOK, http.StatusTooManyRequests}
	if code := e.run("sync"); code != 1 || !strings.Contains(e.err.String(), "rate limited") {
		t.Fatalf("exit %d stderr %q", code, e.err.String())
	}
	st := e.state()
	if len(st.Outbox) != 1500 || st.Outbox[0].Key != "mf-op1000" || st.Cursor != 42 {
		t.Fatalf("first batch's progress must be saved: outbox=%d cursor=%d", len(st.Outbox), st.Cursor)
	}
}

func TestPrimeListsInProgressWorkFirst(t *testing.T) {
	e := newTestEnv(t)
	e.mustRun("init")
	claimed := strings.TrimSpace(e.mustRun("create", "half-done work"))
	e.mustRun("claim", claimed)
	e.mustRun("create", "fresh work")

	out := e.mustRun("prime")
	i, r := strings.Index(out, "## In progress (1)"), strings.Index(out, "## Ready (1)")
	if i < 0 || r < 0 || i > r {
		t.Fatalf("want In progress section before Ready:\n%s", out)
	}
	if !strings.Contains(out, claimed+" [P2] [in_progress] [agent] half-done work") {
		t.Fatalf("claimed task missing from prime:\n%s", out)
	}
}
