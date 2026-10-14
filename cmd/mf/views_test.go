package main

import (
	"strings"
	"testing"
	"time"
)

func TestReadyTasks(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	task := func(key, lane, status string, prio, age int, blockers ...string) *Task {
		return &Task{Key: key, Title: key, Lane: lane, Status: status, Priority: prio,
			CreatedAt: base.Add(time.Duration(age) * time.Minute), BlockedBy: blockers}
	}
	st := &State{Tasks: map[string]*Task{}}
	for _, tk := range []*Task{
		task("mf-done01", "agent", "done", 0, 0),
		task("mf-cncl01", "agent", "canceled", 0, 0),
		task("mf-open01", "agent", "in_progress", 0, 0),
		task("mf-p2old0", "agent", "todo", 2, 1),
		task("mf-p2new0", "agent", "todo", 2, 5),
		task("mf-p0blkd", "agent", "todo", 0, 0, "mf-done01", "mf-cncl01"), // all closed → ready
		task("mf-blocka", "agent", "todo", 0, 0, "mf-done01", "mf-open01"), // one open → not ready
		task("mf-unknwn", "agent", "todo", 0, 0, "mf-ghost0"),              // unknown → unresolved
		task("mf-operat", "operator", "todo", 0, 0),
		task("mf-backlg", "agent", "backlog", 0, 0),
		task("mf-blkst0", "agent", "blocked", 0, 0),
	} {
		st.Tasks[tk.Key] = tk
	}
	var keys []string
	for _, tk := range readyTasks(st) {
		keys = append(keys, tk.Key)
	}
	if got, want := strings.Join(keys, ","), "mf-p0blkd,mf-p2old0,mf-p2new0"; got != want {
		t.Fatalf("ready = %s, want %s", got, want)
	}
	if line := formatLine(st, st.Tasks["mf-blocka"]); !strings.HasSuffix(line, "(blocked by: mf-open01)") {
		t.Fatalf("formatLine = %q", line)
	}
	if inbox := operatorInbox(st); len(inbox) != 1 || inbox[0].Key != "mf-operat" {
		t.Fatalf("operator inbox = %+v", inbox)
	}
}
