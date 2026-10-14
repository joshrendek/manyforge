package main

import (
	"context"
	"strings"
)

const primeRules = `# mf workflow context (ManyForge Work Board)

This repo tracks work with ` + "`mf`" + `. Re-run ` + "`mf prime`" + ` after compaction or a new session.

## Rules

- Use ` + "`mf`" + ` for ALL task tracking. Do NOT use TodoWrite, TaskCreate, or markdown TODO lists.
- Claim before you start: ` + "`mf claim <key>`" + `. Close when done: ` + "`mf close <key> --reason \"…\"`" + `.
- Found follow-up work? ` + "`mf create \"title\"`" + ` (add ` + "`--blocked-by <key>`" + ` for dependencies) instead of a TODO comment.
- **Need a human?** When a step needs a person — credentials, approvals, DNS, payments,
  manual checks you can't automate — create an operator task with
  ` + "`mf create --operator \"exactly what to do and why\"`" + `, then continue with other work.
  Don't stop and wait, and don't bury the ask in chat.

## Commands

    mf ready                         unblocked agent work (priority order)
    mf show <key>                    details + notes (keys can be abbreviated: k3x9)
    mf claim <key>                   status in_progress, assigned to you
    mf close <key>... [--reason …]   mark done
    mf create "title" [-d desc] [-p 0-4] [-t task|bug|feature|chore]
              [--operator] [--status backlog|todo] [--blocked-by k1,k2] [--label a,b]
    mf update <key> [--status …] [-p …] [--title …] [-d …] [--assignee …] [--blocked-by …]
    mf note <key> "text"             append a progress note
    mf list | board | backlog | operator   (add --json for JSON)
    mf sync                          push/pull now (mutations sync automatically)
    mf import beads [file|-] [--all] copy bd issues into mf (open ones by default)
`

// cmdPrime prints the agent workflow context for Claude Code hooks. It must
// never break a session: outside an mf repo it prints nothing, and every
// failure exits 0.
func cmdPrime(a *app, _ []string) error {
	r, err := a.openRepo()
	if err != nil {
		return nil
	}
	if tok, _ := a.canSync(r); tok != "" && !a.noSync {
		ctx, cancel := context.WithTimeout(context.Background(), primeTimeout)
		_, err := a.sync(ctx, r, tok, primeTimeout)
		cancel()
		if err != nil {
			fprintf(a.stderr, "mf: prime: sync skipped: %v\n", err)
		}
	}
	st, err := r.loadState()
	if err != nil {
		fprintf(a.stderr, "mf: prime: %v\n", err)
		return nil
	}
	var b strings.Builder
	b.WriteString(primeRules)
	active := filterTasks(st, func(t *Task) bool { return t.Lane == "agent" && t.Status == "in_progress" })
	if len(active) > 0 {
		fprintf(&b, "\n## In progress (%d) — resume these before picking new work\n\n", len(active))
		printTasks(&b, st, active, "")
	}
	ready := readyTasks(st)
	fprintf(&b, "\n## Ready (%d)\n\n", len(ready))
	printTasks(&b, st, ready[:min(len(ready), 10)], "(nothing ready)")
	if len(ready) > 10 {
		fprintf(&b, "… %d more: run `mf ready`\n", len(ready)-10)
	}
	inbox := operatorInbox(st)
	fprintf(&b, "\n## Operator inbox — waiting on a human (%d)\n\n", len(inbox))
	printTasks(&b, st, inbox, "(empty)")
	if n := len(st.Outbox); n > 0 && r.cfg.Server != "" {
		fprintf(&b, "\n%d local change(s) not yet synced.\n", n)
	}
	fprint(a.stdout, b.String())
	return nil
}
