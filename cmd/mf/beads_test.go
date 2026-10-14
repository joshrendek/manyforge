package main

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func fixturePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("testdata/beads.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportBeadsOpenOnly(t *testing.T) {
	e := newTestEnv(t)
	e.mustRun("init", "--no-docs")
	out := e.mustRun("import", "beads", fixturePath(t))
	if out != "mf: imported 4, skipped 3 (closed 1, existing 0, invalid 2)" {
		t.Fatalf("summary = %q", out)
	}
	if !strings.Contains(e.err.String(), `skipping "Bad Key!"`) || !strings.Contains(e.err.String(), "line 8") {
		t.Fatalf("stderr = %q", e.err.String())
	}
	st := e.state()
	if len(st.Tasks) != 4 || st.Tasks["manyforge-ccc"] != nil {
		t.Fatalf("tasks = %v", slices.Sorted(maps.Keys(st.Tasks)))
	}

	epic := st.Tasks["manyforge-aaa"]
	wantDesc := "Body\n\n## Acceptance criteria\nIt works\n\n## Design\nUse X"
	switch {
	case epic.Type != "feature" || strings.Join(epic.Labels, ",") != "web,epic":
		t.Errorf("epic type/labels: %s %v", epic.Type, epic.Labels)
	case epic.Description != wantDesc:
		t.Errorf("description = %q", epic.Description)
	case strings.Join(epic.BlockedBy, ",") != "manyforge-bbb": // closed blocker dropped, parent-child ignored
		t.Errorf("blocked_by = %v", epic.BlockedBy)
	case epic.Priority != 1 || epic.Status != "todo" || epic.Lane != "agent":
		t.Errorf("epic = %+v", epic)
	case !epic.CreatedAt.Equal(time.Date(2026, 1, 2, 8, 4, 5, 123456000, time.UTC)):
		t.Errorf("created_at = %v", epic.CreatedAt)
	case !strings.Contains(epic.Notes, "bd-import] some notes\n\njosh 2026-01-03T00:00:00Z: first comment"):
		t.Errorf("notes = %q", epic.Notes)
	}
	if b := st.Tasks["manyforge-bbb"]; b.Status != "in_progress" || b.Priority != 2 || b.Type != "task" || b.Assignee != "claude" {
		t.Errorf("bbb = %+v", b)
	}
	if d := st.Tasks["manyforge-n6xh.5"]; d.Status != "backlog" || d.Type != "task" || d.Priority != 3 {
		t.Errorf("deferred decision = %+v", d)
	}
	if b := st.Tasks["manyforge-eee"]; b.Status != "blocked" || b.Type != "chore" || b.Priority != 0 {
		t.Errorf("eee = %+v", b)
	}

	// Outbox: 4 creates + 1 folded note; created_at travels on the wire.
	var kinds []string
	for _, op := range st.Outbox {
		kinds = append(kinds, op.Op+":"+op.Key)
	}
	if strings.Join(kinds, " ") != "create:manyforge-aaa note:manyforge-aaa create:manyforge-bbb create:manyforge-n6xh.5 create:manyforge-eee" {
		t.Fatalf("outbox = %v", kinds)
	}
	wire, _ := json.Marshal(st.Outbox[0])
	if !strings.Contains(string(wire), `"created_at":"2026-01-02T08:04:05.123456Z"`) || strings.Contains(string(wire), "closed_at") {
		t.Fatalf("create op = %s", wire)
	}
	if st.Outbox[1].Author != "bd-import" {
		t.Fatalf("note author = %q", st.Outbox[1].Author)
	}

	// Re-import is a no-op.
	if out := e.mustRun("import", "beads", fixturePath(t)); out != "mf: imported 0, skipped 7 (closed 1, existing 4, invalid 2)" {
		t.Fatalf("re-import = %q", out)
	}
}

func TestImportBeadsAllViaBdExport(t *testing.T) {
	e := newTestEnv(t)
	e.mustRun("init", "--no-docs")
	data, err := os.ReadFile(fixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	var ranIn string
	e.bdExport = func(dir string) ([]byte, error) { ranIn = dir; return data, nil }
	if out := e.mustRun("import", "beads", "--all"); out != "mf: imported 5, skipped 2 (closed 0, existing 0, invalid 2)" {
		t.Fatalf("summary = %q", out)
	}
	if ranIn != e.dir {
		t.Fatalf("bd export ran in %q", ranIn)
	}
	st := e.state()
	c := st.Tasks["manyforge-ccc"]
	if c.Status != "done" || c.Type != "bug" || c.ClosedAt == nil || !c.ClosedAt.Equal(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)) ||
		!strings.Contains(c.Notes, "Close reason: fixed") {
		t.Fatalf("closed issue = %+v", c)
	}
	if bl := st.Tasks["manyforge-aaa"].BlockedBy; strings.Join(bl, ",") != "manyforge-bbb,manyforge-ccc" {
		t.Fatalf("with --all the closed blocker is kept: %v", bl)
	}

	e.bdExport = func(string) ([]byte, error) { return nil, errors.New("bd: not found") }
	if code := e.run("import", "beads"); code != 1 || !strings.Contains(e.err.String(), "bd export failed") {
		t.Fatalf("exit %d stderr %q", code, e.err.String())
	}
}

func TestImportBeadsFromStdin(t *testing.T) {
	e := newTestEnv(t)
	e.mustRun("init", "--no-docs")
	e.stdin = `{"id":"mf-abc","title":"from stdin"}` + "\n"
	if out := e.mustRun("import", "beads", "-"); out != "mf: imported 1, skipped 0 (closed 0, existing 0, invalid 0)" {
		t.Fatalf("summary = %q", out)
	}
	if code := e.run("import", "jira"); code != 1 {
		t.Fatalf("unknown source: exit %d", code)
	}
}
