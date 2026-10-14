package main

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testEnv runs mf in-process against a temp repo with a fake environment, so
// no test touches the real ~/.config or network.
type testEnv struct {
	t        *testing.T
	dir      string
	env      map[string]string
	out, err bytes.Buffer
	clock    time.Time
	stdin    string
	bdExport func(dir string) ([]byte, error)
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	return &testEnv{
		t:     t,
		dir:   dir,
		clock: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
		env: map[string]string{
			"HOME":            filepath.Join(dir, "home"),
			"XDG_CONFIG_HOME": filepath.Join(dir, "xdg"),
			"USER":            "tester",
		},
	}
}

// run executes one mf invocation; stdout/stderr hold only that call's output.
func (e *testEnv) run(args ...string) int {
	e.out.Reset()
	e.err.Reset()
	a := &app{
		stdout: &e.out, stderr: &e.err, stdin: strings.NewReader(e.stdin), bdExport: e.bdExport,
		getenv: func(k string) string { return e.env[k] },
		cwd:    e.dir,
		now: func() time.Time {
			e.clock = e.clock.Add(time.Second) // strictly increasing created_at
			return e.clock
		},
		client: &http.Client{Timeout: 5 * time.Second},
	}
	return a.run(args)
}

func (e *testEnv) mustRun(args ...string) string {
	e.t.Helper()
	if code := e.run(args...); code != 0 {
		e.t.Fatalf("mf %v: exit %d\nstdout: %s\nstderr: %s", args, code, e.out.String(), e.err.String())
	}
	return strings.TrimSpace(e.out.String())
}

// repo opens the test repo with the same state binding mf uses.
func (e *testEnv) repo() (*repo, error) {
	base, err := stateBase(func(k string) string { return e.env[k] })
	if err != nil {
		return nil, err
	}
	return findRepo(e.dir, base)
}

func (e *testEnv) state() *State {
	e.t.Helper()
	r, err := e.repo()
	if err != nil {
		e.t.Fatal(err)
	}
	st, err := r.loadState()
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func TestParseArgsInterspersed(t *testing.T) {
	a := newApp()
	fs := a.newFlags("x", "")
	p := fs.Int("p", 2, "")
	d := fs.String("d", "", "")
	pos, err := parseArgs(fs, []string{"fix", "-p", "1", "the bug", "-d", "desc", "--", "-literal"})
	if err != nil {
		t.Fatal(err)
	}
	if *p != 1 || *d != "desc" || strings.Join(pos, "|") != "fix|the bug|-literal" {
		t.Fatalf("p=%d d=%q pos=%q", *p, *d, pos)
	}
}

func TestExitCodes(t *testing.T) {
	e := newTestEnv(t)
	if code := e.run("ready"); code != 1 || !strings.Contains(e.err.String(), "mf init") {
		t.Fatalf("outside repo: exit %d, stderr %q", code, e.err.String())
	}
	if code := e.run("nope"); code != 1 {
		t.Fatalf("unknown command: exit %d", code)
	}
	e.mustRun("init", "--no-docs")
	if code := e.run("show", "zzzzzz"); code != 2 {
		t.Fatalf("missing key: exit %d", code)
	}
	if code := e.run("create", "x", "-p", "9"); code != 1 {
		t.Fatalf("bad priority: exit %d", code)
	}
	if code := e.run("create", "-d", "no title"); code != 1 {
		t.Fatalf("missing title: exit %d", code)
	}
	if len(e.state().Tasks) != 0 {
		t.Fatal("a rejected create must not be stored")
	}
}

func TestInitAndLocalLifecycle(t *testing.T) {
	e := newTestEnv(t)
	e.dir = filepath.Join(e.dir, "My-Repo.Name")
	mkdir(t, e.dir)
	e.mustRun("init")
	r, _ := e.repo()
	if r.cfg.KeyPrefix != "myrepona" {
		t.Fatalf("prefix = %q", r.cfg.KeyPrefix)
	}
	if got := readFile(t, filepath.Join(e.dir, ".mf", ".gitignore")); got != gitignoreBody {
		t.Fatalf(".gitignore = %q", got)
	}
	if !strings.Contains(readFile(t, filepath.Join(e.dir, "AGENTS.md")), blockBegin) {
		t.Fatal("init should create AGENTS.md with the mf block")
	}

	// create prints the key and nothing else on stdout.
	key := e.mustRun("create", "Write", "the", "docs", "-p", "1", "--label", "docs,cli")
	if !strings.HasPrefix(key, "myrepona-") || len(key) != len("myrepona-")+6 || strings.Contains(key, "\n") {
		t.Fatalf("create stdout = %q", key)
	}
	op := e.mustRun("create", "--operator", "Rotate the prod key")
	st := e.state()
	if st.Tasks[key].Title != "Write the docs" || st.Tasks[key].Priority != 1 || len(st.Tasks[key].Labels) != 2 {
		t.Fatalf("task = %+v", st.Tasks[key])
	}
	if st.Tasks[op].Lane != "operator" || st.Tasks[op].Status != "todo" {
		t.Fatalf("operator task = %+v", st.Tasks[op])
	}
	if len(st.Outbox) != 2 || st.Outbox[0].Op != "create" || *st.Outbox[0].Fields.Title != "Write the docs" {
		t.Fatalf("outbox = %+v", st.Outbox)
	}

	short := key[len(key)-6 : len(key)-2]
	e.mustRun("claim", short)
	if got := e.state().Tasks[key]; got.Status != "in_progress" || got.Assignee != "tester" {
		t.Fatalf("after claim: %+v", got)
	}
	e.env["MF_ACTOR"] = "claude"
	e.mustRun("close", key, "--reason", "shipped")
	got := e.state().Tasks[key]
	if got.Status != "done" || got.ClosedAt == nil || !strings.Contains(got.Notes, "claude] shipped") {
		t.Fatalf("after close: %+v", got)
	}
	e.mustRun("reopen", key)
	if got := e.state().Tasks[key]; got.Status != "todo" || got.ClosedAt != nil {
		t.Fatalf("after reopen: %+v", got)
	}
	e.mustRun("update", key, "--status", "backlog", "--blocked-by", op)
	if got := e.state().Tasks[key]; got.Status != "backlog" || got.BlockedBy[0] != op {
		t.Fatalf("after update: %+v", got)
	}
	if out := e.mustRun("backlog"); !strings.Contains(out, key) || !strings.Contains(out, "(blocked by: "+op+")") {
		t.Fatalf("backlog = %q", out)
	}
	if out := e.mustRun("operator"); !strings.Contains(out, op+" [P2] [todo] [operator] Rotate the prod key") {
		t.Fatalf("operator = %q", out)
	}
	if out := e.mustRun("board"); !strings.Contains(out, "## todo (1)") || !strings.Contains(out, "## done (last 0 of 0)") {
		t.Fatalf("board = %q", out)
	}
	if out := e.mustRun("status"); !strings.Contains(out, "outbox:  7 pending") {
		t.Fatalf("status = %q", out)
	}
}

func TestLoginStoresTokenOutsideRepo(t *testing.T) {
	e := newTestEnv(t)
	e.mustRun("init", "--server", "https://forge.example/", "--project", "mf", "--no-docs")
	if code := e.run("login", "--token", "nope"); code != 1 {
		t.Fatalf("non-mfw token: exit %d", code)
	}
	e.mustRun("login", "--token", "mfw_secret")
	path := filepath.Join(e.env["XDG_CONFIG_HOME"], "mf", "credentials.json")
	if fi := statFile(t, path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %v", fi.Mode().Perm())
	}
	if !strings.Contains(readFile(t, path), `"https://forge.example|mf": "mfw_secret"`) {
		t.Fatalf("credentials = %s", readFile(t, path))
	}
	if strings.Contains(readFile(t, filepath.Join(e.dir, ".mf", "config.json")), "mfw_") {
		t.Fatal("token leaked into the repo")
	}
	r, _ := e.repo()
	var stderr strings.Builder
	a := &app{getenv: func(k string) string { return e.env[k] }, stderr: &stderr}
	if tok, _ := a.token(r); tok != "mfw_secret" {
		t.Fatalf("token() = %q", tok)
	}
	e.env["MF_TOKEN"] = "mfw_env"
	e.env["MF_SERVER"] = "HTTPS://forge.example:443/"
	if tok, src := a.token(r); tok != "mfw_env" || src != "MF_TOKEN" {
		t.Fatalf("bound MF_TOKEN should win: %q %q", tok, src)
	}
}

// A repo's .mf/config.json must not be able to redirect MF_TOKEN to a host of
// its choosing: the env token is only used when MF_SERVER names the same origin.
func TestMFTokenRequiresMatchingMFServer(t *testing.T) {
	srv := newFakeServer(t)
	e := newTestEnv(t)
	e.mustRun("init", "--server", srv.URL, "--no-docs")
	e.env["MF_TOKEN"] = "mfw_env"

	for _, server := range []string{"", "https://forge.example", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)} {
		e.env["MF_SERVER"] = server
		e.mustRun("create", "x")
		if n := len(srv.requests()); n != 0 {
			t.Fatalf("MF_SERVER=%q: token sent to the repo's server (%d requests)", server, n)
		}
		if got := e.err.String(); strings.Count(got, "mf: ignoring MF_TOKEN: set MF_SERVER="+srv.URL+" to bind it") != 1 {
			t.Fatalf("MF_SERVER=%q: want exactly one warning, stderr %q", server, got)
		}
	}

	e.env["MF_SERVER"] = srv.URL + "/"
	e.mustRun("create", "y")
	if reqs := srv.requests(); len(reqs) != 1 || len(reqs[0].Ops) != 4 || srv.auth[0] != "Bearer mfw_env" {
		t.Fatalf("bound token should sync all 4 ops: %d requests", len(reqs))
	}
	if strings.Contains(e.err.String(), "ignoring MF_TOKEN") {
		t.Fatal("no warning expected once bound")
	}
}

func TestSameOrigin(t *testing.T) {
	for _, c := range []struct {
		x, y string
		want bool
	}{
		{"https://forge.example", "https://forge.example/", true},
		{"https://Forge.example:443", "https://forge.example", true},
		{"http://127.0.0.1:8081", "http://127.0.0.1:8081/base", true},
		{"https://forge.example", "http://forge.example", false},
		{"https://forge.example", "https://forge.example:8443", false},
		{"https://forge.example", "https://evil.example", false},
		{"", "", false},
	} {
		if got := sameOrigin(c.x, c.y); got != c.want {
			t.Errorf("sameOrigin(%q, %q) = %v", c.x, c.y, got)
		}
	}
}
