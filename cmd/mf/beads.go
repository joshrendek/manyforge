package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

// beadsIssue is the subset of a `bd export` JSONL line that mf imports.
type beadsIssue struct {
	Kind               string   `json:"_type"`
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	Description        string   `json:"description"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	Design             string   `json:"design"`
	Notes              string   `json:"notes"`
	Status             string   `json:"status"`
	Priority           *int     `json:"priority"`
	IssueType          string   `json:"issue_type"`
	Assignee           string   `json:"assignee"`
	Labels             []string `json:"labels"`
	CreatedAt          string   `json:"created_at"`
	ClosedAt           string   `json:"closed_at"`
	CloseReason        string   `json:"close_reason"`
	Dependencies       []struct {
		IssueID     string `json:"issue_id"`
		DependsOnID string `json:"depends_on_id"`
		Type        string `json:"type"`
	} `json:"dependencies"`
	Comments []struct {
		Author    string `json:"author"`
		Text      string `json:"text"`
		CreatedAt string `json:"created_at"`
	} `json:"comments"`
}

var beadsStatus = map[string]string{
	"": "todo", "open": "todo", "in_progress": "in_progress", "blocked": "blocked",
	"deferred": "backlog", "closed": "done",
}

func cmdImport(a *app, args []string) error {
	fs := a.newFlags("import", "beads [file.jsonl|-] [--all]")
	all := fs.Bool("all", false, "also import closed issues")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || pos[0] != "beads" || len(pos) > 2 {
		return usagef("usage: mf import beads [file.jsonl|-] [--all]")
	}
	r, err := a.openRepo()
	if err != nil {
		return err
	}
	var data []byte
	switch {
	case len(pos) == 1:
		if data, err = a.bdExport(r.root); err != nil {
			return usagef("bd export failed: %v (pass a JSONL file instead)", err)
		}
	case pos[1] == "-":
		data, err = io.ReadAll(a.stdin)
	default:
		data, err = os.ReadFile(pos[1])
	}
	if err != nil {
		return err
	}
	issues, bad := a.parseBeads(data)
	return a.mutate(func(r *repo, st *State) error {
		n, skipped := a.importBeads(st, issues, *all)
		fprintf(a.stdout, "mf: imported %d, skipped %d (closed %d, existing %d, invalid %d)\n",
			n, skipped.total()+bad, skipped.closed, skipped.existing, skipped.invalid+bad)
		return nil
	})
}

// parseBeads decodes JSONL, dropping memory lines; undecodable lines are
// warned about and counted.
func (a *app) parseBeads(data []byte) ([]beadsIssue, int) {
	var issues []beadsIssue
	bad := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for line := 1; sc.Scan(); line++ {
		text := bytes.TrimSpace(sc.Bytes())
		if len(text) == 0 {
			continue
		}
		var is beadsIssue
		if err := json.Unmarshal(text, &is); err != nil {
			fprintf(a.stderr, "mf: import: line %d: %v\n", line, err)
			bad++
			continue
		}
		if is.Kind == "memory" {
			continue
		}
		issues = append(issues, is)
	}
	if err := sc.Err(); err != nil {
		fprintf(a.stderr, "mf: import: %v\n", err)
		bad++
	}
	return issues, bad
}

type importSkips struct{ closed, existing, invalid int }

func (s importSkips) total() int { return s.closed + s.existing + s.invalid }

// importBeads queues a create (plus one folded note) per importable issue.
func (a *app) importBeads(st *State, issues []beadsIssue, all bool) (int, importSkips) {
	var skips importSkips
	// Blockers that were closed in beads and are not being imported would
	// otherwise count as unknown → unresolved and block ready work forever.
	closedSkipped := map[string]bool{}
	for _, is := range issues {
		if is.Status == "closed" && !all {
			closedSkipped[is.ID] = true
		}
	}
	imported := 0
	for _, is := range issues {
		switch {
		case !validKey(is.ID):
			fprintf(a.stderr, "mf: import: skipping %q: not a valid key\n", is.ID)
			skips.invalid++
			continue
		case closedSkipped[is.ID]:
			skips.closed++
			continue
		case st.Tasks[is.ID] != nil:
			skips.existing++
			continue
		}
		p := beadsPatch(is, closedSkipped)
		if err := p.validate(); err != nil {
			fprintf(a.stderr, "mf: import: skipping %s: %v\n", is.ID, err)
			skips.invalid++
			continue
		}
		now := a.now().UTC()
		t := &Task{Key: is.ID, Labels: []string{}, BlockedBy: []string{}, CreatedAt: now}
		p.apply(t, now)
		st.Tasks[is.ID] = t
		st.Outbox = append(st.Outbox, Op{Op: "create", Key: is.ID, Fields: p})
		if note := beadsNote(is); note != "" {
			a.addNoteAs(st, is.ID, note, "bd-import")
		}
		imported++
	}
	return imported, skips
}

func beadsPatch(is beadsIssue, closedSkipped map[string]bool) *patch {
	typ := is.IssueType
	labels := slices.Clone(is.Labels)
	if labels == nil {
		labels = []string{}
	}
	switch typ {
	case "epic":
		typ = "feature"
		if !slices.Contains(labels, "epic") {
			labels = append(labels, "epic")
		}
	case "decision", "":
		typ = "task"
	}
	status, ok := beadsStatus[is.Status]
	if !ok {
		status = "todo"
	}
	prio := 2
	if is.Priority != nil {
		prio = min(max(*is.Priority, 0), 4)
	}
	desc := is.Description
	if s := strings.TrimSpace(is.AcceptanceCriteria); s != "" {
		desc += "\n\n## Acceptance criteria\n" + s
	}
	if s := strings.TrimSpace(is.Design); s != "" {
		desc += "\n\n## Design\n" + s
	}
	blockers := []string{}
	for _, d := range is.Dependencies {
		k := d.DependsOnID
		if d.Type == "blocks" && validKey(k) && !closedSkipped[k] && !slices.Contains(blockers, k) {
			blockers = append(blockers, k)
		}
	}
	p := &patch{
		Title: ptr(is.Title), Description: ptr(strings.TrimSpace(desc)), Lane: ptr("agent"),
		Status: ptr(status), Priority: &prio, Type: &typ, Labels: &labels, BlockedBy: &blockers,
		CreatedAt: parseTime(is.CreatedAt),
	}
	if is.Assignee != "" {
		p.Assignee = ptr(is.Assignee)
	}
	if isClosed(status) {
		p.ClosedAt = parseTime(is.ClosedAt)
	}
	return p
}

// beadsNote folds notes, comments and the close reason into one note.
func beadsNote(is beadsIssue) string {
	var parts []string
	if s := strings.TrimSpace(is.Notes); s != "" {
		parts = append(parts, s)
	}
	for _, c := range is.Comments {
		if s := strings.TrimSpace(c.Text); s != "" {
			parts = append(parts, strings.TrimSpace(c.Author+" "+c.CreatedAt)+": "+s)
		}
	}
	if s := strings.TrimSpace(is.CloseReason); s != "" {
		parts = append(parts, "Close reason: "+s)
	}
	return strings.Join(parts, "\n\n")
}

func parseTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}
