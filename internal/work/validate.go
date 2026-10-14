package work

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Field caps. The DB carries matching CHECKs for the load-bearing ones (title, description,
// notes, assignee, label/blocked_by cardinality); these are the friendly first line.
const (
	maxTitleLen       = 500
	maxDescriptionLen = 20000
	maxNotesLen       = 200000
	maxNoteLen        = 10000
	maxAuthorLen      = 100
	maxAssigneeLen    = 200
	maxLabels         = 50
	maxLabelLen       = 64
	maxBlockedBy      = 100
	maxProjectNameLen = 200
	maxTokenNameLen   = 100

	keySuffixLen = 6
	base36       = "0123456789abcdefghijklmnopqrstuvwxyz"
)

var (
	keyRe    = regexp.MustCompile(`^[a-z][a-z0-9]{0,11}-[a-z0-9]{2,16}(\.[0-9]{1,4}){0,3}$`)
	slugRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	prefixRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,11}$`)
	// tokenRe is the exact shape NewWorkToken mints: mfw_ + 32 random bytes, base64url, unpadded.
	tokenRe = regexp.MustCompile(`^mfw_[A-Za-z0-9_-]{43}$`)

	slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
)

var (
	validLanes    = map[string]bool{"agent": true, "operator": true}
	validStatuses = map[string]bool{"backlog": true, "todo": true, "in_progress": true, "blocked": true, "done": true, "canceled": true}
	validTypes    = map[string]bool{"task": true, "bug": true, "feature": true, "chore": true}
)

// normalizeFields validates every field that was sent and returns a normalized copy: title and
// assignee trimmed, labels trimmed/deduplicated with empties dropped, blocked_by keys
// lowercased/deduplicated. Unsent (nil) fields stay nil.
func normalizeFields(f TaskFields) (TaskFields, error) {
	out := f
	if f.Title != nil {
		t := strings.TrimSpace(*f.Title)
		if t == "" || utf8.RuneCountInString(t) > maxTitleLen {
			return TaskFields{}, errValidation(fmt.Sprintf("title must be 1..%d characters", maxTitleLen))
		}
		out.Title = &t
	}
	if f.Description != nil && utf8.RuneCountInString(*f.Description) > maxDescriptionLen {
		return TaskFields{}, errValidation(fmt.Sprintf("description must be at most %d characters", maxDescriptionLen))
	}
	if f.Lane != nil && !validLanes[*f.Lane] {
		return TaskFields{}, errValidation("lane must be agent or operator")
	}
	if f.Status != nil && !validStatuses[*f.Status] {
		return TaskFields{}, errValidation("status must be one of backlog, todo, in_progress, blocked, done, canceled")
	}
	if f.Priority != nil && (*f.Priority < 0 || *f.Priority > 4) {
		return TaskFields{}, errValidation("priority must be 0..4")
	}
	if f.Type != nil && !validTypes[*f.Type] {
		return TaskFields{}, errValidation("type must be one of task, bug, feature, chore")
	}
	if f.Assignee != nil {
		a := strings.TrimSpace(*f.Assignee)
		if utf8.RuneCountInString(a) > maxAssigneeLen {
			return TaskFields{}, errValidation(fmt.Sprintf("assignee must be at most %d characters", maxAssigneeLen))
		}
		out.Assignee = &a
	}
	if f.Labels != nil {
		labels := dedupe(*f.Labels, strings.TrimSpace)
		if len(labels) > maxLabels {
			return TaskFields{}, errValidation(fmt.Sprintf("at most %d labels", maxLabels))
		}
		for _, l := range labels {
			if utf8.RuneCountInString(l) > maxLabelLen {
				return TaskFields{}, errValidation(fmt.Sprintf("labels must be at most %d characters", maxLabelLen))
			}
		}
		out.Labels = &labels
	}
	if f.BlockedBy != nil {
		keys := dedupe(*f.BlockedBy, func(s string) string { return strings.ToLower(strings.TrimSpace(s)) })
		if len(keys) > maxBlockedBy {
			return TaskFields{}, errValidation(fmt.Sprintf("at most %d blocked_by keys", maxBlockedBy))
		}
		for _, k := range keys {
			if !keyRe.MatchString(k) {
				return TaskFields{}, errValidation("blocked_by must contain task keys")
			}
		}
		out.BlockedBy = &keys
	}
	return out, nil
}

// dedupe normalizes each value, drops empties, and keeps the first occurrence. The result is
// never nil, so an explicit [] clears the column rather than reading as "not sent".
func dedupe(in []string, norm func(string) string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		v = norm(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// taskValues is a fully-populated task for insert. createdAt/closedAt are only set by an
// `mf` import (nil = now).
type taskValues struct {
	title, description, lane, status, typ, assignee string
	priority                                        int16
	labels, blockedBy                               []string
	createdAt, closedAt                             *time.Time
}

// createValues applies the create-time defaults to normalized fields: lane agent, status
// todo, priority 2, type task. Title is required.
func createValues(f TaskFields) (taskValues, error) {
	if f.Title == nil {
		return taskValues{}, errValidation("title required")
	}
	v := taskValues{
		title: *f.Title, lane: "agent", status: "todo", priority: 2, typ: "task",
		labels: []string{}, blockedBy: []string{},
	}
	if f.Description != nil {
		v.description = *f.Description
	}
	if f.Lane != nil {
		v.lane = *f.Lane
	}
	if f.Status != nil {
		v.status = *f.Status
	}
	if f.Priority != nil {
		v.priority = int16(*f.Priority)
	}
	if f.Type != nil {
		v.typ = *f.Type
	}
	if f.Assignee != nil {
		v.assignee = *f.Assignee
	}
	if f.Labels != nil {
		v.labels = *f.Labels
	}
	if f.BlockedBy != nil {
		v.blockedBy = *f.BlockedBy
	}
	return v, nil
}

// validKey reports whether key is a well-formed task key in the project's namespace.
func validKey(key, prefix string) bool {
	return keyRe.MatchString(key) && strings.HasPrefix(key, prefix+"-")
}

// newTaskKey mints "<prefix>-<6 base36>" from crypto/rand (the web UI's server-side keys; `mf`
// mints the same shape client-side for offline creates).
func newTaskKey(prefix string) (string, error) {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteByte('-')
	max := big.NewInt(int64(len(base36)))
	for i := 0; i < keySuffixLen; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("work: key entropy: %w", err)
		}
		b.WriteByte(base36[n.Int64()])
	}
	return b.String(), nil
}

// formatNote validates a note and renders the entry appended to a task's notes:
// "[<RFC3339 UTC> <author>] <text>". author falls back to fallback when blank; control
// characters (including newlines) in the author are flattened so an entry header is one line.
func formatNote(now time.Time, author, fallback, via, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > maxNoteLen {
		return "", errValidation(fmt.Sprintf("note text must be 1..%d characters", maxNoteLen))
	}
	author = strings.Join(strings.FieldsFunc(author, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '[' || r == ']'
	}), " ")
	if utf8.RuneCountInString(author) > maxAuthorLen {
		return "", errValidation(fmt.Sprintf("note author must be at most %d characters", maxAuthorLen))
	}
	if author == "" {
		author = fallback
	}
	if via != "" {
		author += " (" + via + ")"
	}
	// Continuation lines are indented so only a real entry header ever starts a line with
	// "[": a note body cannot forge "[<ts> <someone else>] approved" as a separate entry.
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	text = strings.ReplaceAll(text, "\n", "\n  ")
	return fmt.Sprintf("[%s %s] %s", now.UTC().Format(time.RFC3339), author, text), nil
}

// normalizeProject validates the create payload and derives slug (from name) and key_prefix
// (from slug) when they are omitted.
func normalizeProject(in ProjectInput) (name, slug, prefix string, err error) {
	name = strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > maxProjectNameLen {
		return "", "", "", errValidation(fmt.Sprintf("name must be 1..%d characters", maxProjectNameLen))
	}
	slug = strings.ToLower(strings.TrimSpace(in.Slug))
	if slug == "" {
		slug = slugify(name)
	}
	if !slugRe.MatchString(slug) {
		return "", "", "", errValidation("slug must match ^[a-z0-9][a-z0-9-]{0,62}$")
	}
	// {pid} accepts an id or a slug, so a slug that parses as a UUID would be ambiguous.
	if _, perr := uuid.Parse(slug); perr == nil {
		return "", "", "", errValidation("slug must not be a UUID")
	}
	prefix = strings.ToLower(strings.TrimSpace(in.KeyPrefix))
	if prefix == "" {
		prefix = deriveKeyPrefix(slug)
	}
	if !prefixRe.MatchString(prefix) {
		return "", "", "", errValidation("key_prefix must match ^[a-z][a-z0-9]{0,11}$")
	}
	return name, slug, prefix, nil
}

// slugify lowercases, collapses runs of non-alphanumerics to one hyphen, trims hyphens, and
// caps the result at 63 characters.
func slugify(s string) string {
	s = slugNonAlnum.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

// deriveKeyPrefix takes the first ≤8 alphanumerics of the slug after any leading digits (a
// prefix must start with a letter); an all-digit slug falls back to "w".
func deriveKeyPrefix(slug string) string {
	var b strings.Builder
	for _, r := range slug {
		if b.Len() == 8 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9' && b.Len() > 0:
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "w"
	}
	return b.String()
}

// validTokenName trims and caps a token label.
func validTokenName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxTokenNameLen {
		return "", errValidation(fmt.Sprintf("name must be 1..%d characters", maxTokenNameLen))
	}
	return name, nil
}
