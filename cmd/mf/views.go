package main

import (
	"cmp"
	"encoding/json"
	"io"
	"slices"
	"strings"
)

// unresolvedBlockers lists t's blockers that are not done/canceled. A key the
// local store doesn't know counts as unresolved.
func unresolvedBlockers(st *State, t *Task) []string {
	var out []string
	for _, k := range t.BlockedBy {
		if b, ok := st.Tasks[k]; !ok || !isClosed(b.Status) {
			out = append(out, k)
		}
	}
	return out
}

// readyTasks is the agent lane's pick-up list: todo with every blocker closed.
func readyTasks(st *State) []*Task {
	return filterTasks(st, func(t *Task) bool {
		return t.Lane == "agent" && t.Status == "todo" && len(unresolvedBlockers(st, t)) == 0
	})
}

func operatorInbox(st *State) []*Task {
	return filterTasks(st, func(t *Task) bool { return t.Lane == "operator" && !isClosed(t.Status) })
}

// filterTasks returns matching tasks sorted by priority, then age, then key.
func filterTasks(st *State, keep func(*Task) bool) []*Task {
	out := []*Task{}
	for _, t := range st.Tasks {
		if keep(t) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(x, y *Task) int {
		return cmp.Or(cmp.Compare(x.Priority, y.Priority), x.CreatedAt.Compare(y.CreatedAt), cmp.Compare(x.Key, y.Key))
	})
	return out
}

// formatLine is the compact one-task-per-line form every listing uses:
// mf-k3x9q2 [P1] [todo] [agent] Title  @alice  (blocked by: mf-aaaaaa)
func formatLine(st *State, t *Task) string {
	var b strings.Builder
	fprintf(&b, "%s [P%d] [%s] [%s] %s", t.Key, t.Priority, t.Status, t.Lane, t.Title)
	if t.Assignee != "" {
		fprintf(&b, "  @%s", t.Assignee)
	}
	if bl := unresolvedBlockers(st, t); len(bl) > 0 && !isClosed(t.Status) {
		fprintf(&b, "  (blocked by: %s)", strings.Join(bl, ", "))
	}
	return b.String()
}

func printTasks(w io.Writer, st *State, tasks []*Task, empty string) {
	if len(tasks) == 0 && empty != "" {
		fprintln(w, empty)
	}
	for _, t := range tasks {
		fprintln(w, formatLine(st, t))
	}
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printTask(w io.Writer, st *State, t *Task) {
	fprintln(w, formatLine(st, t))
	field := func(name, val string) {
		if val != "" {
			fprintf(w, "  %-11s %s\n", name+":", val)
		}
	}
	field("type", t.Type)
	field("labels", strings.Join(t.Labels, ", "))
	field("blocked_by", strings.Join(t.BlockedBy, ", "))
	field("created", t.CreatedAt.Format("2006-01-02 15:04"))
	field("updated", t.UpdatedAt.Format("2006-01-02 15:04"))
	if t.ClosedAt != nil {
		field("closed", t.ClosedAt.Format("2006-01-02 15:04"))
	}
	if t.Description != "" {
		fprintf(w, "\n%s\n", strings.TrimSpace(t.Description))
	}
	if notes := strings.TrimSpace(t.Notes); notes != "" {
		fprintf(w, "\nnotes:\n%s\n", notes)
	}
}
