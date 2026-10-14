package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const gitignoreBody = "state.json\nstate.lock\n*.tmp\n"

func (a *app) openRepo() (*repo, error) { return findRepo(a.cwd) }

func (a *app) readState() (*repo, *State, error) {
	r, err := a.openRepo()
	if err != nil {
		return nil, nil, err
	}
	st, err := r.loadState()
	return r, st, err
}

// mutate applies fn under the state lock (fn appends its ops to the outbox),
// then makes the best-effort sync.
func (a *app) mutate(fn func(r *repo, st *State) error) error {
	r, err := a.openRepo()
	if err != nil {
		return err
	}
	// Buffer output so nothing is printed for a change that wasn't saved.
	var out bytes.Buffer
	stdout := a.stdout
	a.stdout = &out
	err = r.withState(func(st *State) error { return fn(r, st) })
	a.stdout = stdout
	if err != nil {
		return err
	}
	_, _ = out.WriteTo(stdout)
	a.autoSync(r)
	return nil
}

// actor names whoever is running mf, for claims and note authorship.
func (a *app) actor() string {
	for _, k := range []string{"MF_ACTOR", "USER"} {
		if v := a.getenv(k); v != "" {
			return v
		}
	}
	return "agent"
}

// gitRoot is the nearest ancestor holding .git, else start itself.
func gitRoot(start string) string {
	abs, err := filepath.Abs(start)
	if err != nil {
		return start
	}
	for dir := abs; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		if filepath.Dir(dir) == dir {
			return abs
		}
	}
}

func cmdInit(a *app, args []string) error {
	fs := a.newFlags("init", "[--server URL] [--project SLUG] [--prefix P] [--no-docs]")
	server := fs.String("server", "", "ManyForge base URL")
	project := fs.String("project", "", "work project slug")
	prefix := fs.String("prefix", "", "task key prefix (default: repo dir name)")
	noDocs := fs.Bool("no-docs", false, "don't touch AGENTS.md/CLAUDE.md")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	r, err := a.openRepo()
	if errors.Is(err, errNoRepo) {
		r, err = &repo{root: gitRoot(a.cwd)}, nil
	}
	if err != nil {
		return err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["server"] {
		r.cfg.Server = strings.TrimRight(*server, "/")
	}
	if set["project"] {
		r.cfg.Project = *project
	}
	if set["prefix"] {
		if !prefixRE.MatchString(strings.ToLower(*prefix)) {
			return usagef("prefix must be a letter followed by up to 11 of [a-z0-9]")
		}
		r.cfg.KeyPrefix = strings.ToLower(*prefix)
	}
	if r.cfg.KeyPrefix == "" {
		r.cfg.KeyPrefix = sanitizePrefix(filepath.Base(r.root))
	}
	if err := os.MkdirAll(r.dir(), 0o755); err != nil {
		return err
	}
	if err := r.saveConfig(); err != nil {
		return err
	}
	if err := writeFileAtomic(r.path(".gitignore"), []byte(gitignoreBody), 0o644); err != nil {
		return err
	}
	fprintf(a.stdout, "mf: initialized %s (prefix %q)\n", r.dir(), r.cfg.KeyPrefix)
	if !*noDocs {
		changed, err := upsertAgentDocs(r.root)
		if err != nil {
			return err
		}
		for _, p := range changed {
			fprintf(a.stdout, "mf: updated %s\n", p)
		}
	}
	if r.cfg.Server != "" {
		if tok, _ := a.token(r); tok == "" {
			fprintln(a.stdout, "mf: next: mf login --token mfw_…  (create a token under Work in ManyForge)")
		}
	}
	return nil
}

func cmdLogin(a *app, args []string) error {
	fs := a.newFlags("login", "--token mfw_… (or - to read stdin) [--server URL] [--project SLUG]")
	tok := fs.String("token", "", "work token, or - to read it from stdin")
	server := fs.String("server", "", "server (default: from .mf/config.json)")
	project := fs.String("project", "", "project (default: from .mf/config.json)")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	fromRepo := false
	if r, err := a.openRepo(); err == nil {
		switch {
		case *server == "":
			*server, fromRepo = r.cfg.Server, true
		case r.cfg.Server != "" && !sameOrigin(*server, r.cfg.Server):
			// The token is sent wherever .mf/config.json points; refuse to bind it
			// to a server other than the one the operator named.
			return usagef("--server %s does not match this repo's server %s (.mf/config.json)", *server, r.cfg.Server)
		}
		if *project == "" {
			*project = r.cfg.Project
		}
	}
	if *server == "" {
		return usagef("no server: pass --server or run `mf init --server URL` first")
	}
	if *tok == "-" {
		line, err := bufio.NewReader(a.stdin).ReadString('\n')
		if err != nil && line == "" {
			return usagef("no token on stdin")
		}
		*tok = line
	}
	*tok = strings.TrimSpace(*tok)
	if !strings.HasPrefix(*tok, "mfw_") {
		return usagef("expected a work token starting with mfw_ (--token)")
	}
	path, err := a.saveToken(*server, *project, *tok)
	if err != nil {
		return err
	}
	fprintf(a.stdout, "mf: token saved for %s project %q in %s\n", strings.TrimRight(*server, "/"), *project, path)
	if fromRepo {
		fprintf(a.stderr, "mf: the server came from .mf/config.json — make sure %s is your ManyForge instance (pass --server to pin it)\n", strings.TrimRight(*server, "/"))
	}
	return nil
}

// readFlags registers --json for read commands.
func readFlags(a *app, name, usage string) (*flag.FlagSet, *bool) {
	fs := a.newFlags(name, usage)
	return fs, fs.Bool("json", false, "emit JSON")
}

func cmdReady(a *app, args []string) error {
	fs, asJSON := readFlags(a, "ready", "[--json]")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	_, st, err := a.readState()
	if err != nil {
		return err
	}
	tasks := readyTasks(st)
	if *asJSON {
		return printJSON(a.stdout, tasks)
	}
	printTasks(a.stdout, st, tasks, "(nothing ready)")
	return nil
}

func cmdList(a *app, args []string) error {
	fs, asJSON := readFlags(a, "list", "[--status s1,s2] [--lane agent|operator] [--all] [--json]")
	status := fs.String("status", "", "only these statuses (comma-separated)")
	lane := fs.String("lane", "", "only this lane")
	all := fs.Bool("all", false, "include done and canceled")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	want := splitList(*status)
	for _, s := range want {
		if !slices.Contains(statuses, s) {
			return usagef("status must be one of %s", strings.Join(statuses, ", "))
		}
	}
	if *lane != "" && !slices.Contains(lanes, *lane) {
		return usagef("lane must be one of %s", strings.Join(lanes, ", "))
	}
	_, st, err := a.readState()
	if err != nil {
		return err
	}
	tasks := filterTasks(st, func(t *Task) bool {
		switch {
		case *lane != "" && t.Lane != *lane:
			return false
		case len(want) > 0:
			return slices.Contains(want, t.Status)
		default:
			return *all || !isClosed(t.Status)
		}
	})
	slices.SortStableFunc(tasks, func(x, y *Task) int {
		return slices.Index(listOrder, x.Status) - slices.Index(listOrder, y.Status)
	})
	if *asJSON {
		return printJSON(a.stdout, tasks)
	}
	printTasks(a.stdout, st, tasks, "(no tasks)")
	return nil
}

// listOrder puts work in flight first in `mf list`.
var listOrder = []string{"in_progress", "todo", "blocked", "backlog", "done", "canceled"}

func cmdBoard(a *app, args []string) error {
	fs, asJSON := readFlags(a, "board", "[--json]")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	_, st, err := a.readState()
	if err != nil {
		return err
	}
	cols := map[string][]*Task{}
	for _, s := range []string{"todo", "in_progress", "blocked"} {
		cols[s] = filterTasks(st, func(t *Task) bool { return t.Status == s })
	}
	done := filterTasks(st, func(t *Task) bool { return t.Status == "done" })
	closedAt := func(t *Task) time.Time {
		if t.ClosedAt != nil {
			return *t.ClosedAt
		}
		return t.UpdatedAt
	}
	slices.SortFunc(done, func(x, y *Task) int { return closedAt(y).Compare(closedAt(x)) })
	cols["done"] = done[:min(len(done), 10)]
	if *asJSON {
		return printJSON(a.stdout, cols)
	}
	for i, s := range []string{"todo", "in_progress", "blocked", "done"} {
		if i > 0 {
			fprintln(a.stdout)
		}
		label := fmt.Sprintf("%s (%d)", s, len(cols[s]))
		if s == "done" {
			label = fmt.Sprintf("done (last %d of %d)", len(cols[s]), len(done))
		}
		fprintf(a.stdout, "## %s\n", label)
		printTasks(a.stdout, st, cols[s], "")
	}
	return nil
}

func cmdBacklog(a *app, args []string) error {
	fs, asJSON := readFlags(a, "backlog", "[--json]")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	_, st, err := a.readState()
	if err != nil {
		return err
	}
	tasks := filterTasks(st, func(t *Task) bool { return t.Status == "backlog" })
	if *asJSON {
		return printJSON(a.stdout, tasks)
	}
	printTasks(a.stdout, st, tasks, "(backlog is empty)")
	return nil
}

func cmdOperator(a *app, args []string) error {
	fs, asJSON := readFlags(a, "operator", "[--json]")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	_, st, err := a.readState()
	if err != nil {
		return err
	}
	tasks := operatorInbox(st)
	if *asJSON {
		return printJSON(a.stdout, tasks)
	}
	printTasks(a.stdout, st, tasks, "(operator inbox is empty)")
	return nil
}

func cmdShow(a *app, args []string) error {
	fs, asJSON := readFlags(a, "show", "<key> [--json]")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: mf show <key>")
	}
	_, st, err := a.readState()
	if err != nil {
		return err
	}
	key, err := resolveKey(st.Tasks, pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(a.stdout, st.Tasks[key])
	}
	printTask(a.stdout, st, st.Tasks[key])
	return nil
}

// resolveBlockers expands abbreviated blocker keys. A key the local store
// doesn't know is kept verbatim (it may exist on the server) with a warning.
func (a *app) resolveBlockers(st *State, in []string) ([]string, error) {
	out := []string{}
	for _, k := range in {
		key, err := resolveKey(st.Tasks, k)
		var ce *cliError
		if errors.As(err, &ce) && ce.code == 2 && validKey(strings.ToLower(k)) {
			fprintf(a.stderr, "mf: warning: blocker %s is not in the local store (treated as unresolved)\n", k)
			key, err = strings.ToLower(k), nil
		}
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, key) {
			out = append(out, key)
		}
	}
	return out, nil
}

// fieldFlags registers the task-field flags shared by create and update.
type fieldFlags struct {
	title, desc, lane, status, typ, assignee, blockedBy, labels *string
	priority                                                    *int
}

func addFieldFlags(fs *flag.FlagSet) *fieldFlags {
	f := &fieldFlags{
		title:     fs.String("title", "", "title"),
		desc:      fs.String("d", "", "description"),
		lane:      fs.String("lane", "agent", "agent | operator"),
		status:    fs.String("status", "todo", "status"),
		typ:       fs.String("t", "task", "task | bug | feature | chore"),
		assignee:  fs.String("assignee", "", "assignee"),
		blockedBy: fs.String("blocked-by", "", "comma-separated blocker keys"),
		labels:    fs.String("label", "", "comma-separated labels"),
		priority:  fs.Int("p", 2, "priority 0 (critical) - 4"),
	}
	fs.StringVar(f.desc, "description", "", "description")
	fs.StringVar(f.typ, "type", "task", "type")
	fs.IntVar(f.priority, "priority", 2, "priority")
	return f
}

// toPatch builds a patch from the flags that were set (all of them when
// every is true, as for create).
func (f *fieldFlags) toPatch(a *app, fs *flag.FlagSet, st *State, every bool) (*patch, error) {
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	has := func(names ...string) bool {
		return every || slices.ContainsFunc(names, func(n string) bool { return set[n] })
	}
	p := &patch{}
	if has("title") {
		p.Title = f.title
	}
	if has("d", "description") {
		p.Description = f.desc
	}
	if has("lane") {
		p.Lane = f.lane
	}
	if has("status") {
		p.Status = f.status
	}
	if has("t", "type") {
		p.Type = f.typ
	}
	if has("p", "priority") {
		p.Priority = f.priority
	}
	if set["assignee"] || (every && *f.assignee != "") {
		p.Assignee = f.assignee
	}
	if has("label") {
		labels := splitList(*f.labels)
		p.Labels = &labels
	}
	if has("blocked-by") {
		keys, err := a.resolveBlockers(st, splitList(*f.blockedBy))
		if err != nil {
			return nil, err
		}
		p.BlockedBy = &keys
	}
	return p, p.validate()
}

func cmdCreate(a *app, args []string) error {
	fs := a.newFlags("create", `"title" [-d desc] [-p 0-4] [-t task|bug|feature|chore] [--lane agent|operator] [--operator] [--status backlog|todo] [--blocked-by k1,k2] [--label a,b]`)
	f := addFieldFlags(fs)
	operator := fs.Bool("operator", false, "shorthand for --lane operator (a task for a human)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if t := strings.TrimSpace(strings.Join(pos, " ")); t != "" {
		*f.title = t
	}
	if *operator {
		*f.lane = "operator"
	}
	if *f.status != "backlog" && *f.status != "todo" {
		return usagef("create: --status must be backlog or todo")
	}
	return a.mutate(func(r *repo, st *State) error {
		p, err := f.toPatch(a, fs, st, true)
		if err != nil {
			return err
		}
		key, err := newKey(r.keyPrefix(st), st.Tasks)
		if err != nil {
			return err
		}
		now := a.now().UTC()
		t := &Task{Key: key, Labels: []string{}, BlockedBy: []string{}, CreatedAt: now}
		p.apply(t, now)
		st.Tasks[key] = t
		st.Outbox = append(st.Outbox, Op{Op: "create", Key: key, Fields: p})
		fprintln(a.stdout, key)
		return nil
	})
}

func cmdUpdate(a *app, args []string) error {
	fs := a.newFlags("update", "<key> [--status s] [-p n] [--title t] [-d desc] [-t type] [--assignee a] [--lane l] [--blocked-by k1,k2] [--label a,b]")
	f := addFieldFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: mf update <key> [flags]")
	}
	if fs.NFlag() == 0 {
		return usagef("update: nothing to change")
	}
	return a.mutate(func(r *repo, st *State) error {
		p, err := f.toPatch(a, fs, st, false)
		if err != nil {
			return err
		}
		return a.patchTask(st, pos[0], p)
	})
}

// patchTask applies p to the task locally, queues the update op and prints
// the task's new line.
func (a *app) patchTask(st *State, in string, p *patch) error {
	key, err := resolveKey(st.Tasks, in)
	if err != nil {
		return err
	}
	p.apply(st.Tasks[key], a.now().UTC())
	st.Outbox = append(st.Outbox, Op{Op: "update", Key: key, Fields: p})
	fprintln(a.stdout, formatLine(st, st.Tasks[key]))
	return nil
}

// addNote appends a note by the current actor.
func (a *app) addNote(st *State, key, text string) { a.addNoteAs(st, key, text, a.actor()) }

// addNoteAs mirrors the server's note format ("[RFC3339 author] text" entries
// joined by newlines) locally and queues the note op.
func (a *app) addNoteAs(st *State, key, text, author string) {
	t := st.Tasks[key]
	if t.Notes != "" {
		t.Notes += "\n"
	}
	t.Notes += fmt.Sprintf("[%s %s] %s", a.now().UTC().Format(time.RFC3339), author, text)
	t.UpdatedAt = a.now().UTC()
	st.Outbox = append(st.Outbox, Op{Op: "note", Key: key, Text: text, Author: author})
}

func ptr[T any](v T) *T { return &v }

func cmdClaim(a *app, args []string) error {
	fs := a.newFlags("claim", "<key>")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: mf claim <key>")
	}
	return a.mutate(func(r *repo, st *State) error {
		return a.patchTask(st, pos[0], &patch{Status: ptr("in_progress"), Assignee: ptr(a.actor())})
	})
}

func cmdClose(a *app, args []string) error {
	fs := a.newFlags("close", `<key>... [--reason "…"]`)
	reason := fs.String("reason", "", "why (appended as a note)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(`usage: mf close <key>... [--reason "…"]`)
	}
	return a.mutate(func(r *repo, st *State) error {
		for _, in := range pos {
			key, err := resolveKey(st.Tasks, in)
			if err != nil {
				return err
			}
			if *reason != "" {
				a.addNote(st, key, *reason)
			}
			if err := a.patchTask(st, key, &patch{Status: ptr("done")}); err != nil {
				return err
			}
		}
		return nil
	})
}

func cmdReopen(a *app, args []string) error {
	fs := a.newFlags("reopen", "<key>")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: mf reopen <key>")
	}
	return a.mutate(func(r *repo, st *State) error {
		return a.patchTask(st, pos[0], &patch{Status: ptr("todo")})
	})
}

func cmdNote(a *app, args []string) error {
	fs := a.newFlags("note", `<key> "text"`)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef(`usage: mf note <key> "text"`)
	}
	text := strings.TrimSpace(strings.Join(pos[1:], " "))
	if text == "" {
		return usagef("note: text must not be empty")
	}
	return a.mutate(func(r *repo, st *State) error {
		key, err := resolveKey(st.Tasks, pos[0])
		if err != nil {
			return err
		}
		a.addNote(st, key, text)
		fprintln(a.stdout, formatLine(st, st.Tasks[key]))
		return nil
	})
}

// cmdSync always attempts the round trip — an explicit request overrides
// --no-sync / MF_OFFLINE.
func cmdSync(a *app, args []string) error {
	fs := a.newFlags("sync", "")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	r, err := a.openRepo()
	if err != nil {
		return err
	}
	tok, reason := a.canSync(r)
	if tok == "" {
		return usagef("cannot sync: %s", reason)
	}
	sum, err := a.sync(context.Background(), r, tok, syncTimeout)
	if err != nil {
		return err
	}
	fprintf(a.stdout, "mf: synced: pushed %d op(s) (%d failed), pulled %d task(s) in %d request(s), cursor %d\n",
		sum.sent, sum.failed, sum.pulled, sum.requests, sum.cursor)
	return nil
}

func cmdStatus(a *app, args []string) error {
	fs, asJSON := readFlags(a, "status", "[--json]")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}
	r, st, err := a.readState()
	if err != nil {
		return err
	}
	_, tokSource := a.token(r)
	counts := map[string]int{}
	for _, t := range st.Tasks {
		counts[t.Status]++
	}
	if *asJSON {
		return printJSON(a.stdout, map[string]any{
			"root": r.root, "server": r.cfg.Server, "project": r.cfg.Project, "key_prefix": r.keyPrefix(st),
			"token": tokSource != "", "cursor": st.Cursor, "outbox": len(st.Outbox), "counts": counts,
		})
	}
	server := r.cfg.Server
	if server == "" {
		server = "(none — local only)"
	}
	if tokSource == "" {
		tokSource = "none"
	}
	fprintf(a.stdout, "repo:    %s\nserver:  %s\nproject: %s\nprefix:  %s\ntoken:   %s\ncursor:  %d\noutbox:  %d pending op(s)\n",
		r.root, server, r.cfg.Project, r.keyPrefix(st), tokSource, st.Cursor, len(st.Outbox))
	var parts []string
	for _, s := range statuses {
		parts = append(parts, fmt.Sprintf("%s %d", s, counts[s]))
	}
	fprintf(a.stdout, "tasks:   %s\n", strings.Join(parts, ", "))
	return nil
}

func cmdSetup(a *app, args []string) error {
	if len(args) != 1 || args[0] != "claude" {
		return usagef("usage: mf setup claude")
	}
	root := gitRoot(a.cwd)
	if r, err := a.openRepo(); err == nil {
		root = r.root
	}
	path := filepath.Join(root, ".claude", "settings.json")
	changed, err := installClaudeHooks(root, path)
	if err != nil {
		return err
	}
	if changed {
		fprintf(a.stdout, "mf: added `mf prime` SessionStart/PreCompact hooks to %s\n", path)
	} else {
		fprintf(a.stdout, "mf: %s already runs `mf prime`\n", path)
	}
	return nil
}

func cmdVersion(a *app, _ []string) error {
	fprintf(a.stdout, "mf %s\n", version)
	return nil
}

func cmdHelp(a *app, _ []string) error {
	a.printHelp(a.stdout)
	return nil
}
