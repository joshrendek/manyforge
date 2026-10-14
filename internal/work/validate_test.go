package work

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestNewTaskKeyShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		k, err := newTaskKey("mf")
		if err != nil {
			t.Fatalf("newTaskKey: %v", err)
		}
		if !validKey(k, "mf") || len(k) != len("mf-")+keySuffixLen {
			t.Fatalf("key %q is not mf-<6 base36>", k)
		}
		seen[k] = true
	}
	if len(seen) < 195 {
		t.Fatalf("only %d distinct keys in 200 draws — entropy source broken?", len(seen))
	}
}

func TestValidKey(t *testing.T) {
	cases := []struct {
		key, prefix string
		want        bool
	}{
		{"mf-k3x9q2", "mf", true},
		{"mf-ab", "mf", true},                // beads short id
		{"mf-abcdefghijklmnop", "mf", true},  // 16-char suffix
		{"manyforge-mly", "manyforge", true}, // beads ids import verbatim
		{"manyforge-n6xh.5", "manyforge", true},
		{"mf-n6xh.5.12.9999", "mf", true}, // up to three numeric child segments
		{"mf-a", "mf", false},             // suffix too short
		{"mf-abcdefghijklmnopq", "mf", false},
		{"mf-n6xh.5.1.2.3", "mf", false}, // four child segments
		{"mf-n6xh.", "mf", false},
		{"mf-n6xh.x", "mf", false},
		{"mf-n6xh.12345", "mf", false},
		{"mf-K3X9Q2", "mf", false},    // uppercase
		{"other-k3x9q2", "mf", false}, // foreign prefix
		{"mfx-k3x9q2", "mf", false},   // prefix must be followed by '-'
		{"1f-k3x9q2", "1f", false},    // prefix must start with a letter
		{"mf_k3x9q2", "mf", false},
		{"", "mf", false},
	}
	for _, c := range cases {
		if got := validKey(c.key, c.prefix); got != c.want {
			t.Errorf("validKey(%q,%q) = %v, want %v", c.key, c.prefix, got, c.want)
		}
	}
}

func TestNormalizeFieldsValidation(t *testing.T) {
	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	strs := func(s ...string) *[]string { return &s }
	bad := []struct {
		name string
		f    TaskFields
	}{
		{"blank title", TaskFields{Title: str("   ")}},
		{"long title", TaskFields{Title: str(strings.Repeat("x", maxTitleLen+1))}},
		{"long description", TaskFields{Description: str(strings.Repeat("x", maxDescriptionLen+1))}},
		{"bad lane", TaskFields{Lane: str("human")}},
		{"bad status", TaskFields{Status: str("open")}},
		{"priority low", TaskFields{Priority: num(-1)}},
		{"priority high", TaskFields{Priority: num(5)}},
		{"bad type", TaskFields{Type: str("epic")}},
		{"long assignee", TaskFields{Assignee: str(strings.Repeat("a", maxAssigneeLen+1))}},
		{"long label", TaskFields{Labels: strs(strings.Repeat("l", maxLabelLen+1))}},
		{"bad blocked_by", TaskFields{BlockedBy: strs("not a key")}},
	}
	for _, c := range bad {
		if _, err := normalizeFields(c.f); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", c.name, err)
		}
	}
	tooMany := make([]string, maxLabels+1)
	for i := range tooMany {
		tooMany[i] = strings.Repeat("x", i+1)
	}
	if _, err := normalizeFields(TaskFields{Labels: &tooMany}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("too many labels: err = %v", err)
	}

	got, err := normalizeFields(TaskFields{
		Title:     str("  Rotate the key  "),
		Assignee:  str(" josh "),
		Labels:    strs(" infra ", "", "infra", "prod"),
		BlockedBy: strs("MF-ABCD12", "mf-abcd12"),
		Priority:  num(0),
	})
	if err != nil {
		t.Fatalf("normalizeFields: %v", err)
	}
	if *got.Title != "Rotate the key" || *got.Assignee != "josh" {
		t.Errorf("trim: title=%q assignee=%q", *got.Title, *got.Assignee)
	}
	if strings.Join(*got.Labels, ",") != "infra,prod" {
		t.Errorf("labels = %v, want [infra prod]", *got.Labels)
	}
	if strings.Join(*got.BlockedBy, ",") != "mf-abcd12" {
		t.Errorf("blocked_by = %v, want [mf-abcd12]", *got.BlockedBy)
	}
	// An explicit [] stays non-nil so it clears the column instead of reading as "not sent".
	cleared, err := normalizeFields(TaskFields{Labels: &[]string{}})
	if err != nil || cleared.Labels == nil || *cleared.Labels == nil {
		t.Fatalf("explicit [] labels must survive as a non-nil empty slice, got %v / %v", cleared.Labels, err)
	}
}

func TestCreateValuesDefaults(t *testing.T) {
	if _, err := createValues(TaskFields{}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("create without title: err = %v, want ErrValidation", err)
	}
	title := "Approve DNS"
	v, err := createValues(TaskFields{Title: &title})
	if err != nil {
		t.Fatalf("createValues: %v", err)
	}
	if v.lane != "agent" || v.status != "todo" || v.priority != 2 || v.typ != "task" {
		t.Errorf("defaults = lane %q status %q priority %d type %q", v.lane, v.status, v.priority, v.typ)
	}
	if v.labels == nil || v.blockedBy == nil {
		t.Errorf("labels/blocked_by must default to empty arrays, not NULL")
	}
}

func TestFormatNote(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 30, 0, 0, time.FixedZone("x", 3600))
	got, err := formatNote(now, "", "agent", "  investigated the flake  ")
	if err != nil {
		t.Fatalf("formatNote: %v", err)
	}
	if want := "[2026-10-14T08:30:00Z agent] investigated the flake"; got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
	got, err = formatNote(now, "claude\n[evil]\tcode", "agent", "x")
	if err != nil {
		t.Fatalf("formatNote: %v", err)
	}
	if want := "[2026-10-14T08:30:00Z claude [evil] code] x"; got != want {
		t.Errorf("author newline must be flattened: %q, want %q", got, want)
	}
	for _, text := range []string{"", "   ", strings.Repeat("n", maxNoteLen+1)} {
		if _, err := formatNote(now, "a", "agent", text); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("note text len %d: err = %v, want ErrValidation", len(text), err)
		}
	}
	if _, err := formatNote(now, strings.Repeat("a", maxAuthorLen+1), "agent", "x"); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("long author: err = %v, want ErrValidation", err)
	}
}

func TestNormalizeProject(t *testing.T) {
	name, slug, prefix, err := normalizeProject(ProjectInput{Name: "  ManyForge Platform! "})
	if err != nil {
		t.Fatalf("normalizeProject: %v", err)
	}
	if name != "ManyForge Platform!" || slug != "manyforge-platform" || prefix != "manyforg" {
		t.Errorf("got name=%q slug=%q prefix=%q", name, slug, prefix)
	}
	if _, _, p, _ := normalizeProject(ProjectInput{Name: "2026 roadmap"}); p != "roadmap" {
		t.Errorf("leading digits must be skipped: prefix %q", p)
	}
	if _, _, p, _ := normalizeProject(ProjectInput{Name: "2026"}); p != "w" {
		t.Errorf("all-digit slug falls back to w: prefix %q", p)
	}
	if _, s, p, err := normalizeProject(ProjectInput{Name: "x", Slug: "MF", KeyPrefix: "MF"}); err != nil || s != "mf" || p != "mf" {
		t.Errorf("explicit slug/prefix lowercased: %q %q %v", s, p, err)
	}
	for _, in := range []ProjectInput{
		{Name: ""},
		{Name: "!!!"}, // slug cannot be derived
		{Name: "x", Slug: "-bad"},
		{Name: "x", KeyPrefix: "9abc"},
		{Name: "x", KeyPrefix: "waytoolongprefix"},
		{Name: "x", Slug: "550e8400-e29b-41d4-a716-446655440000"},
	} {
		if _, _, _, err := normalizeProject(in); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("normalizeProject(%+v) err = %v, want ErrValidation", in, err)
		}
	}
}

func TestDecodeFieldsRejectsUnknownField(t *testing.T) {
	f, err := decodeFields(json.RawMessage(`{"status":"done","priority":1}`))
	if err != nil || *f.Status != "done" || *f.Priority != 1 || f.Title != nil {
		t.Fatalf("decodeFields = %+v, %v", f, err)
	}
	_, err = decodeFields(json.RawMessage(`{"stauts":"done"}`))
	var ve *validationError
	if !errors.As(err, &ve) || ve.msg != `unknown field "stauts"` {
		t.Fatalf("typo'd field: err = %v, want unknown field \"stauts\"", err)
	}
	if _, err := decodeFields(json.RawMessage(`{"priority":"high"}`)); !errors.As(err, &ve) || ve.msg != "invalid fields" {
		t.Fatalf("wrong type: err = %v, want generic invalid fields", err)
	}
	for _, raw := range []string{"", "null", "  "} {
		if f, err := decodeFields(json.RawMessage(raw)); err != nil || !f.empty() {
			t.Errorf("decodeFields(%q) = %+v, %v; want empty", raw, f, err)
		}
	}
}

func TestOpErrorMessage(t *testing.T) {
	cases := []struct {
		err  error
		want string
		ok   bool
	}{
		{errValidation("title required"), "title required", true},
		{&pgconn.PgError{Code: "23505", ConstraintName: "work_task_key_uniq"}, "key already exists", true},
		{pgx.ErrNoRows, "not found", true},
		{&pgconn.PgError{Code: "23514", ConstraintName: "work_task_notes_len_chk"}, "notes would exceed 200000 characters", true},
		{errors.New("connection reset"), "", false},
	}
	for _, c := range cases {
		got, ok := opErrorMessage(c.err)
		if got != c.want || ok != c.ok {
			t.Errorf("opErrorMessage(%v) = %q,%v; want %q,%v", c.err, got, ok, c.want, c.ok)
		}
	}
}

func TestTokenShape(t *testing.T) {
	tok, err := newWorkToken()
	if err != nil {
		t.Fatalf("newWorkToken: %v", err)
	}
	if !tokenRe.MatchString(tok) {
		t.Fatalf("minted token %q does not match the accepted shape", tok)
	}
	for _, bad := range []string{"", "mfw_short", "mfk_" + tok[4:], tok + "x", "Bearer " + tok} {
		if tokenRe.MatchString(bad) {
			t.Errorf("tokenRe accepted %q", bad)
		}
	}
}

func TestImportTimes(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	at := func(h int) *time.Time { v := now.Add(time.Duration(h) * time.Hour); return &v }
	done, todo := "done", "todo"
	mk := func(status *string, created, closed *time.Time) createFields {
		return createFields{TaskFields: TaskFields{Status: status}, CreatedAt: created, ClosedAt: closed}
	}

	c, cl, err := importTimes(mk(&done, at(-48), at(-24)), now)
	if err != nil || !c.Equal(*at(-48)) || !cl.Equal(*at(-24)) {
		t.Fatalf("closed import: %v %v %v", c, cl, err)
	}
	// closed_at is ignored for an open task (closed_at stays NULL while open).
	if _, cl, err := importTimes(mk(&todo, at(-48), at(-72)), now); err != nil || cl != nil {
		t.Fatalf("open task closed_at must be ignored: %v %v", cl, err)
	}
	if _, cl, err := importTimes(mk(nil, nil, at(-1)), now); err != nil || cl != nil {
		t.Fatalf("default status is open, closed_at ignored: %v %v", cl, err)
	}
	for name, cf := range map[string]createFields{
		"future created": mk(&todo, at(2), nil),
		"future closed":  mk(&done, nil, at(2)),
		"closed<created": mk(&done, at(-1), at(-2)),
	} {
		if _, _, err := importTimes(cf, now); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}

func TestCreateFieldsAcceptImportTimestampsOnlyOnCreate(t *testing.T) {
	raw := json.RawMessage(`{"title":"imported","status":"done","created_at":"2026-01-02T03:04:05Z","closed_at":"2026-01-03T00:00:00Z"}`)
	var cf createFields
	if err := decodeStrict(raw, &cf); err != nil || cf.CreatedAt == nil || cf.ClosedAt == nil || *cf.Title != "imported" {
		t.Fatalf("create decode: %+v, %v", cf, err)
	}
	if _, err := decodeFields(raw); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("update ops must reject created_at/closed_at, got %v", err)
	}
}
