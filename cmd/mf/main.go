// Command mf is the ManyForge Work Board CLI (spec 018): a light, local-first
// task tracker for coding agents. Every repo keeps its tasks in .mf/state.json,
// mutations queue in an outbox, and — when a server and token are configured —
// the outbox syncs with POST {server}/api/v1/work/sync.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// version is stamped at build time: -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(newApp().run(os.Args[1:]))
}

// app carries everything a command touches in the outside world so tests can
// swap it: streams, environment, working directory, clock and HTTP client.
type app struct {
	stdout, stderr io.Writer
	stdin          io.Reader
	getenv         func(string) string
	cwd            string
	now            func() time.Time
	client         *http.Client
	noSync         bool
	warnedEnvToken bool
	// bdExport returns `bd export` output for `mf import beads` (swapped in tests).
	bdExport func(dir string) ([]byte, error)
}

func newApp() *app {
	cwd, _ := os.Getwd()
	return &app{
		stdout: os.Stdout, stderr: os.Stderr, stdin: os.Stdin,
		getenv: os.Getenv, cwd: cwd, now: time.Now, client: &http.Client{},
		bdExport: func(dir string) ([]byte, error) {
			cmd := exec.Command("bd", "export")
			cmd.Dir = dir
			cmd.Stderr = os.Stderr
			return cmd.Output()
		},
	}
}

// cliError carries a process exit code: 1 usage/user error, 2 not found.
type cliError struct {
	code int
	msg  string
}

func (e *cliError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &cliError{code: 1, msg: fmt.Sprintf(format, args...)}
}

func notFoundf(format string, args ...any) error {
	return &cliError{code: 2, msg: fmt.Sprintf(format, args...)}
}

type command struct {
	run     func(a *app, args []string) error
	summary string
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"init":     {cmdInit, "create .mf/ here and add the mf block to AGENTS.md/CLAUDE.md"},
		"login":    {cmdLogin, "store a work token (mfw_…) for this repo's server+project"},
		"prime":    {cmdPrime, "print workflow context for agent hooks (never fails)"},
		"ready":    {cmdReady, "agent-lane todo tasks with no open blockers"},
		"list":     {cmdList, "list open tasks [--status s] [--lane l] [--all]"},
		"board":    {cmdBoard, "active board: todo / in_progress / blocked / recent done"},
		"backlog":  {cmdBacklog, "tasks in the backlog"},
		"operator": {cmdOperator, "open operator-lane tasks (the human inbox)"},
		"show":     {cmdShow, "show one task"},
		"create":   {cmdCreate, "create a task; prints its key"},
		"update":   {cmdUpdate, "update fields of a task"},
		"claim":    {cmdClaim, "mark a task in_progress and assign it to you"},
		"close":    {cmdClose, "mark tasks done [--reason …]"},
		"reopen":   {cmdReopen, "move a done/canceled task back to todo"},
		"note":     {cmdNote, "append a note to a task"},
		"sync":     {cmdSync, "push the outbox and pull changes now"},
		"status":   {cmdStatus, "show config, sync state and counts"},
		"import":   {cmdImport, "import beads [file.jsonl|-] [--all]: copy bd issues into mf"},
		"setup":    {cmdSetup, "setup claude: add mf prime to .claude/settings.json hooks"},
		"version":  {cmdVersion, "print the mf version"},
		"help":     {cmdHelp, "show this help"},
	}
}

func (a *app) run(args []string) int {
	var rest []string
	for _, arg := range args {
		if arg == "--no-sync" {
			a.noSync = true
			continue
		}
		rest = append(rest, arg)
	}
	if v := a.getenv("MF_OFFLINE"); v != "" && v != "0" {
		a.noSync = true
	}
	if len(rest) == 0 {
		a.printHelp(a.stderr)
		return 1
	}
	name := rest[0]
	switch name {
	case "-h", "--help":
		name = "help"
	case "--version":
		name = "version"
	}
	cmd, ok := commands[name]
	if !ok {
		fprintf(a.stderr, "mf: unknown command %q (run `mf help`)\n", name)
		return 1
	}
	if err := cmd.run(a, rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fprintf(a.stderr, "mf: %v\n", err)
		var ce *cliError
		if errors.As(err, &ce) {
			return ce.code
		}
		return 1
	}
	return 0
}

func (a *app) printHelp(w io.Writer) {
	fprintln(w, "mf — ManyForge Work Board CLI")
	fprintln(w, "\nusage: mf [--no-sync] <command> [args]")
	fprintln(w, "\ncommands:")
	for _, name := range []string{
		"init", "login", "prime", "ready", "list", "board", "backlog", "operator", "show",
		"create", "update", "claim", "close", "reopen", "note", "sync", "status", "import", "setup", "version", "help",
	} {
		fprintf(w, "  %-9s %s\n", name, commands[name].summary)
	}
	fprintln(w, "\nenvironment: MF_TOKEN + MF_SERVER (work token, used only when MF_SERVER matches the repo server), MF_ACTOR (claim/note author), MF_OFFLINE=1 (skip sync)")
}

// newFlags builds a FlagSet that reports errors through run() instead of
// printing its own copy; -h prints the command's usage line.
func (a *app) newFlags(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() { fprintf(a.stderr, "usage: mf %s %s\n", name, usage) }
	return fs
}

// parseArgs parses flags interspersed with positional arguments (Go's flag
// package stops at the first positional), so `mf create "title" -p 1` works.
// Everything after a literal "--" is positional.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usagef("%s: %v", fs.Name(), err)
		}
		rest := fs.Args()
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// splitList parses a comma-separated flag value; "" yields an empty list.
func splitList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// fprintf and friends write CLI output; a failed write to stdout/stderr has
// nowhere better to be reported, so the error is deliberately dropped.
func fprintf(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
func fprintln(w io.Writer, args ...any)               { _, _ = fmt.Fprintln(w, args...) }
func fprint(w io.Writer, args ...any)                 { _, _ = fmt.Fprint(w, args...) }
