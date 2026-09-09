# Agent Instructions

This project uses **bd** (Beads) in a separately configured **private store**. Run `bd info` first and confirm it resolves that store.

## Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work atomically
bd close <id>         # Complete work
bd dolt push          # Sync only to the configured private Beads remote
```

## Private Tracking Requirements

- Use `bd` for task tracking and persistent knowledge. Never commit issue records,
  memories, operational receipts, database files, or personal notes to this repository.
- The local `.beads/redirect` and `.beads/config.yaml` are ignored routing/safety
  files, not project data. If private routing is missing, ask the operator to
  configure it; do not initialize a new task database in the application checkout.
- Keep `export.auto` and `export.git-add` disabled in application checkouts.
  Do not install Beads hooks that export or stage task data.
- Never run an export into the application repository. Commit JSONL snapshots and
  native backups only from the private tracking checkout using its sync workflow.
- Keep credentials out of task descriptions, even in the private store.
- Do not merge pre-scrub history into this repository. Re-clone or rebase work
  onto the sanitized history; an old merge can republish removed private data.

## Non-Interactive Shell Commands

**ALWAYS use non-interactive flags** with file operations to avoid hanging on confirmation prompts.

Shell commands like `cp`, `mv`, and `rm` may be aliased to include `-i` (interactive) mode on some systems, causing the agent to hang indefinitely waiting for y/n input.

**Use these forms instead:**
```bash
# Force overwrite without prompting
cp -f source dest           # NOT: cp source dest
mv -f source dest           # NOT: mv source dest
rm -f file                  # NOT: rm file

# For recursive operations
rm -rf directory            # NOT: rm -r directory
cp -rf source dest          # NOT: cp -r source dest
```

**Other commands that may prompt:**
- `scp` - use `-o BatchMode=yes` for non-interactive
- `ssh` - use `-o BatchMode=yes` to fail instead of prompting
- `apt-get` - use `-y` flag
- `brew` - use `HOMEBREW_NO_AUTO_UPDATE=1` env var

## Session Completion

1. Record remaining work and completed tasks in the private Beads store.
2. Run relevant quality gates when application code changes.
3. Commit and push application changes only; confirm the application checkout is clean.
4. Sync private task data separately from its private tracking checkout.
5. Preserve other users' work, stashes, and branches during cleanup.
6. Hand off verification results and any remaining blockers without private record dumps.
