package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Enumerations from the spec's data model.
var (
	lanes    = []string{"agent", "operator"}
	statuses = []string{"backlog", "todo", "in_progress", "blocked", "done", "canceled"}
	types    = []string{"task", "bug", "feature", "chore"}
)

func isClosed(status string) bool { return status == "done" || status == "canceled" }

// Task is the wire format shared with the server (spec 018 "Wire format").
type Task struct {
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Notes       string     `json:"notes"`
	Lane        string     `json:"lane"`
	Status      string     `json:"status"`
	Priority    int        `json:"priority"`
	Type        string     `json:"type"`
	Assignee    string     `json:"assignee"`
	Labels      []string   `json:"labels"`
	BlockedBy   []string   `json:"blocked_by"`
	Rev         int64      `json:"rev"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
}

type Project struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
}

// patch is the set of fields an op changes. Nil pointers are "not sent", so the
// server patches only what the caller touched.
type patch struct {
	Title       *string   `json:"title,omitempty"`
	Description *string   `json:"description,omitempty"`
	Lane        *string   `json:"lane,omitempty"`
	Status      *string   `json:"status,omitempty"`
	Priority    *int      `json:"priority,omitempty"`
	Type        *string   `json:"type,omitempty"`
	Assignee    *string   `json:"assignee,omitempty"`
	Labels      *[]string `json:"labels,omitempty"`
	BlockedBy   *[]string `json:"blocked_by,omitempty"`
	// Only sent on create (imports preserve the original timestamps).
	CreatedAt *time.Time `json:"created_at,omitempty"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
}

func (p *patch) validate() error {
	switch {
	case p.Title != nil && strings.TrimSpace(*p.Title) == "":
		return usagef("title must not be empty")
	case p.Lane != nil && !slices.Contains(lanes, *p.Lane):
		return usagef("lane must be one of %s", strings.Join(lanes, ", "))
	case p.Status != nil && !slices.Contains(statuses, *p.Status):
		return usagef("status must be one of %s", strings.Join(statuses, ", "))
	case p.Type != nil && !slices.Contains(types, *p.Type):
		return usagef("type must be one of %s", strings.Join(types, ", "))
	case p.Priority != nil && (*p.Priority < 0 || *p.Priority > 4):
		return usagef("priority must be 0-4")
	}
	return nil
}

// apply mirrors the server's update semantics locally, including closed_at
// stamping when status enters or leaves done/canceled.
func (p *patch) apply(t *Task, now time.Time) {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&t.Title, p.Title)
	set(&t.Description, p.Description)
	set(&t.Lane, p.Lane)
	set(&t.Type, p.Type)
	set(&t.Assignee, p.Assignee)
	if p.Priority != nil {
		t.Priority = *p.Priority
	}
	if p.Labels != nil {
		t.Labels = *p.Labels
	}
	if p.BlockedBy != nil {
		t.BlockedBy = *p.BlockedBy
	}
	if p.CreatedAt != nil {
		t.CreatedAt = *p.CreatedAt
	}
	if p.ClosedAt != nil {
		t.ClosedAt = p.ClosedAt
	}
	if p.Status != nil {
		t.Status = *p.Status
		switch {
		case isClosed(t.Status) && t.ClosedAt == nil:
			t.ClosedAt = &now
		case !isClosed(t.Status):
			t.ClosedAt = nil
		}
	}
	t.UpdatedAt = now
}

// Op is one queued mutation, in the sync request's wire format.
type Op struct {
	Op     string `json:"op"` // create | update | note
	Key    string `json:"key"`
	Fields *patch `json:"fields,omitempty"`
	Text   string `json:"text,omitempty"`
	Author string `json:"author,omitempty"`
}

// State is <stateDir>/state.json: the local cache, the sync cursor and the outbox.
type State struct {
	Cursor  int64            `json:"cursor"`
	Project *Project         `json:"project,omitempty"`
	Tasks   map[string]*Task `json:"tasks"`
	Outbox  []Op             `json:"outbox"`
}

// Config is .mf/config.json (committed).
type Config struct {
	Server    string `json:"server"`
	Project   string `json:"project"`
	KeyPrefix string `json:"key_prefix"`
}

// repo is a directory containing .mf/config.json. Its local state (cache,
// cursor, outbox) lives OUTSIDE the checkout in stateDir: a cloned repo must
// never be able to seed the outbox that mf replays with the operator's token.
type repo struct {
	root     string
	cfg      Config
	stateDir string
}

var errNoRepo = errors.New("not in an mf repo (no .mf/config.json here or above); run `mf init`")

func (r *repo) dir() string                  { return filepath.Join(r.root, ".mf") }
func (r *repo) path(name string) string      { return filepath.Join(r.dir(), name) }
func (r *repo) statePath(name string) string { return filepath.Join(r.stateDir, name) }

// stateBase is $XDG_STATE_HOME/mf, defaulting to ~/.local/state/mf.
func stateBase(getenv func(string) string) (string, error) {
	if d := getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "mf"), nil
	}
	if h := getenv("HOME"); h != "" {
		return filepath.Join(h, ".local", "state", "mf"), nil
	}
	return "", errors.New("cannot locate mf state: set HOME or XDG_STATE_HOME")
}

// stateDirFor names a checkout's state directory: readable dir name plus a
// hash of the resolved root, so two checkouts never share an outbox.
func stateDirFor(base, root string) string {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		real = root
	}
	sum := sha256.Sum256([]byte(real))
	return filepath.Join(base, sanitizePrefix(filepath.Base(real))+"-"+hex.EncodeToString(sum[:6]))
}

// findRepo walks up from start to the nearest .mf/config.json, like git, and
// binds the repo's state directory under base.
func findRepo(start, base string) (*repo, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, ".mf", "config.json"))
		if err == nil {
			r := &repo{root: dir, stateDir: stateDirFor(base, dir)}
			if err := json.Unmarshal(data, &r.cfg); err != nil {
				return nil, fmt.Errorf("parse .mf/config.json: %w", err)
			}
			r.cfg.Server = strings.TrimRight(r.cfg.Server, "/")
			return r, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, errNoRepo
		}
		dir = parent
	}
}

func (r *repo) saveConfig() error {
	return writeJSONAtomic(r.path("config.json"), r.cfg, 0o644)
}

// loadState reads state.json without locking: writes are tmp+rename, so a
// reader always sees a complete file.
func (r *repo) loadState() (*State, error) {
	st := &State{}
	data, err := os.ReadFile(r.statePath("state.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, st); err != nil {
			return nil, fmt.Errorf("parse %s: %w", r.statePath("state.json"), err)
		}
	}
	if st.Tasks == nil {
		st.Tasks = map[string]*Task{}
	}
	return st, nil
}

// withState runs a read-modify-write of state.json under the repo lock. The
// state is saved only if fn succeeds.
func (r *repo) withState(fn func(*State) error) error {
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	unlock, err := lockFile(r.statePath("state.lock"))
	if err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer unlock()
	st, err := r.loadState()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return writeJSONAtomic(r.statePath("state.json"), st, 0o600)
}

// keyPrefix prefers the server's project prefix once known, so offline-created
// keys match what the server validates.
func (r *repo) keyPrefix(st *State) string {
	if st.Project != nil && st.Project.KeyPrefix != "" {
		return st.Project.KeyPrefix
	}
	if r.cfg.KeyPrefix != "" {
		return r.cfg.KeyPrefix
	}
	return "mf"
}

// keyRE is the accepted key format (beads-compatible: `manyforge-n6xh.5`).
// Generated keys are always <prefix>-<6 base36>.
var (
	keyRE    = regexp.MustCompile(`^[a-z][a-z0-9]{0,11}-[a-z0-9]{2,16}(\.[0-9]{1,4}){0,3}$`)
	prefixRE = regexp.MustCompile(`^[a-z][a-z0-9]{0,11}$`)
)

func validKey(k string) bool { return keyRE.MatchString(k) }

const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

// newKey returns <prefix>-<6 base36> from crypto/rand, unique among tasks.
func newKey(prefix string, tasks map[string]*Task) (string, error) {
	for {
		var b strings.Builder
		buf := make([]byte, 1)
		for b.Len() < 6 {
			if _, err := rand.Read(buf); err != nil {
				return "", err
			}
			if buf[0] < 252 { // 252 = 7*36: reject the tail to avoid modulo bias
				b.WriteByte(base36[buf[0]%36])
			}
		}
		key := prefix + "-" + b.String()
		if _, taken := tasks[key]; !taken {
			return key, nil
		}
	}
}

// resolveKey finds a task by exact key, or by a unique abbreviation: a prefix
// or suffix of the key, or a prefix of its random part (`k3x9` → `mf-k3x9q2`).
func resolveKey(tasks map[string]*Task, in string) (string, error) {
	in = strings.ToLower(strings.TrimSpace(in))
	if _, ok := tasks[in]; ok {
		return in, nil
	}
	var matches []string
	if in != "" {
		for key := range tasks {
			id := key[strings.LastIndex(key, "-")+1:]
			if strings.HasPrefix(key, in) || strings.HasSuffix(key, in) || strings.HasPrefix(id, in) {
				matches = append(matches, key)
			}
		}
	}
	switch len(matches) {
	case 0:
		return "", notFoundf("no task matches %q", in)
	case 1:
		return matches[0], nil
	default:
		slices.Sort(matches)
		return "", usagef("%q is ambiguous: %s", in, strings.Join(matches, ", "))
	}
}

// sanitizePrefix derives a key prefix from a directory name: lowercase
// [a-z0-9] starting with a letter, at most 8 characters.
func sanitizePrefix(name string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(name) {
		if (c >= 'a' && c <= 'z') || (b.Len() > 0 && c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
		if b.Len() == 8 {
			break
		}
	}
	if b.Len() == 0 {
		return "mf"
	}
	return b.String()
}

// credentials: ~/.config/mf/credentials.json maps "<server>|<project>" to a
// work token. It lives outside the repo so a token is never committed.
func (a *app) credentialsPath() (string, error) {
	if dir := a.getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "mf", "credentials.json"), nil
	}
	home := a.getenv("HOME")
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(home, ".config", "mf", "credentials.json"), nil
}

// sameOrigin compares scheme, host and port (default ports filled in);
// paths and trailing slashes are ignored.
func sameOrigin(x, y string) bool {
	origin := func(s string) string {
		u, err := url.Parse(strings.TrimSpace(s))
		if err != nil || u.Host == "" {
			return ""
		}
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
		}
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port
	}
	ox := origin(x)
	return ox != "" && ox == origin(y)
}

func credKey(server, project string) string { return strings.TrimRight(server, "/") + "|" + project }

func (a *app) loadCredentials() (map[string]string, string, error) {
	path, err := a.credentialsPath()
	if err != nil {
		return nil, "", err
	}
	creds := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, path, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &creds); err != nil {
			return nil, path, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	return creds, path, nil
}

// token returns the work token for r and where it came from ("" if none).
//
// .mf/config.json is repo-controlled, so a hostile repo could point "server"
// anywhere. MF_TOKEN is therefore only used when MF_SERVER names the same
// origin as the repo's server; the credentials file is keyed by server already.
func (a *app) token(r *repo) (tok, source string) {
	if t := strings.TrimSpace(a.getenv("MF_TOKEN")); t != "" {
		if sameOrigin(a.getenv("MF_SERVER"), r.cfg.Server) {
			return t, "MF_TOKEN"
		}
		if !a.warnedEnvToken {
			a.warnedEnvToken = true
			fprintf(a.stderr, "mf: ignoring MF_TOKEN: set MF_SERVER=%s to bind it\n", r.cfg.Server)
		}
	}
	creds, path, err := a.loadCredentials()
	if err != nil {
		return "", ""
	}
	if t := creds[credKey(r.cfg.Server, r.cfg.Project)]; t != "" {
		return t, path
	}
	return "", ""
}

func (a *app) saveToken(server, project, tok string) (string, error) {
	creds, path, err := a.loadCredentials()
	if err != nil {
		return "", err
	}
	creds[credKey(server, project)] = tok
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, writeJSONAtomic(path, creds, 0o600)
}

func writeJSONAtomic(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), perm)
}

// writeFileAtomic writes via a temp file in the same directory + rename, so a
// crash never leaves a truncated file behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
