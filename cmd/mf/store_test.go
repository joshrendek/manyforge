package main

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func statFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func TestNewKey(t *testing.T) {
	re := regexp.MustCompile(`^mf-[0-9a-z]{6}$`)
	tasks := map[string]*Task{}
	for range 500 {
		k, err := newKey("mf", tasks)
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(k) {
			t.Fatalf("bad key %q", k)
		}
		if tasks[k] != nil {
			t.Fatalf("duplicate key %q", k)
		}
		tasks[k] = &Task{}
	}
}

func TestValidKey(t *testing.T) {
	for k, want := range map[string]bool{
		"mf-k3x9q2": true, "manyforge-mly": true, "manyforge-n6xh.5": true, "abc-12.3.4.5": true,
		"a-zz": true, "abcdefghijkl-zz": true,
		"abcdefghijklm-zz": false, // prefix > 12
		"1mf-abc":          false, "mf-a": false, "MF-abc": false, "mf_abc": false,
		"mf-abc.1.2.3.4": false, "mf-abc.": false, "mf-abc-def": false, "": false,
	} {
		if got := validKey(k); got != want {
			t.Errorf("validKey(%q) = %v", k, got)
		}
	}
	tasks := map[string]*Task{}
	for range 100 {
		k, _ := newKey("manyforge", tasks)
		if !validKey(k) {
			t.Fatalf("generated key %q is invalid", k)
		}
	}
}

func TestResolveKey(t *testing.T) {
	tasks := map[string]*Task{"mf-k3x9q2": {}, "mf-k3abcd": {}, "mf-zz0001": {}}
	for in, want := range map[string]string{
		"mf-k3x9q2": "mf-k3x9q2", // exact
		"K3X9":      "mf-k3x9q2", // prefix of the random part, case-insensitive
		"9q2":       "mf-k3x9q2", // suffix
		"mf-k3x":    "mf-k3x9q2", // prefix of the full key
		"zz":        "mf-zz0001",
	} {
		got, err := resolveKey(tasks, in)
		if err != nil || got != want {
			t.Errorf("resolveKey(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	var ce *cliError
	_, err := resolveKey(tasks, "k3")
	if !errors.As(err, &ce) || ce.code != 1 || !strings.Contains(err.Error(), "mf-k3abcd, mf-k3x9q2") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := resolveKey(tasks, "mf-"); !errors.As(err, &ce) || ce.code != 1 {
		t.Errorf("bare prefix should be ambiguous: %v", err)
	}
	_, err = resolveKey(tasks, "nothere")
	if !errors.As(err, &ce) || ce.code != 2 {
		t.Errorf("not found: %v", err)
	}
	if _, err := resolveKey(tasks, ""); err == nil {
		t.Error("empty input must not match everything")
	}
}

func TestSanitizePrefix(t *testing.T) {
	for in, want := range map[string]string{
		"manyforge": "manyforg", "My-Repo": "myrepo", "___": "mf", "ab12": "ab12", "42things": "things", "123": "mf",
	} {
		if got := sanitizePrefix(in); got != want {
			t.Errorf("sanitizePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindRepoWalksUp(t *testing.T) {
	e := newTestEnv(t)
	e.mustRun("init", "--no-docs")
	sub := e.dir + "/a/b"
	mkdir(t, sub)
	r, err := findRepo(sub)
	if err != nil || r.root != e.dir {
		t.Fatalf("findRepo = %+v, %v", r, err)
	}
}

func TestPatchApplyClosedAt(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	task := &Task{Status: "todo"}
	(&patch{Status: ptr("done")}).apply(task, now)
	if task.ClosedAt == nil || !task.ClosedAt.Equal(now) {
		t.Fatalf("done should stamp closed_at: %+v", task)
	}
	(&patch{Status: ptr("canceled")}).apply(task, now.Add(time.Hour))
	if !task.ClosedAt.Equal(now) {
		t.Fatal("done→canceled keeps the original closed_at")
	}
	(&patch{Status: ptr("todo")}).apply(task, now)
	if task.ClosedAt != nil {
		t.Fatal("reopening clears closed_at")
	}
}
