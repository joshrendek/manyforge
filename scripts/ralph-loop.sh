#!/usr/bin/env bash
# Fresh-context Ralph driver for Spec 004 connectors (US4 -> US5 -> US6).
#
# WHY: the in-session ralph-loop plugin re-feeds the prompt in the SAME context,
# so context only grows. This driver instead spawns a SEPARATE `claude -p` process
# per increment — each starts near-empty, reads HANDOFF.md, does ONE increment,
# commits, and exits. Context resets every increment (stays well under 30%).
#
# Usage:  scripts/ralph-loop.sh [MAX_INCREMENTS]   (default 40)
# Stop:   Ctrl-C, or `pkill -f ralph-loop.sh`. Durable state (bd + HANDOFF.md +
#         committed plans) means a re-run resumes cleanly.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
export PATH="$PATH:$HOME/go/bin"

MAX="${1:-40}"
LOG="ralph-driver.log"

# One concise instruction per fresh process. ALL the cadence/gotchas live in HANDOFF.md.
PROMPT='Read HANDOFF.md in this repo, then perform EXACTLY ONE increment of its "Autonomous loop cadence" section, advancing epic manyforge-a7j through US4 then US5 then US6 in numeric order. Use the subagent-driven cadence: a fresh implementer subagent (TDD), then a spec-compliance review subagent, then a code-quality review subagent; triage findings rigorously; trust `go build`/`go test` over gopls false positives about dbgen and integration files. Commit the increment (--no-verify, no Co-Authored-By). Keep HANDOFF.md current. Then STOP and exit — do NOT begin a second increment.'

echo "[driver] start $(date) — max $MAX increments" | tee -a "$LOG"

is_done() {
  # US4, US5, US6 all closed?
  local closed; closed="$(bd list --status closed 2>/dev/null)"
  echo "$closed" | grep -q 'a7j\.4' && echo "$closed" | grep -q 'a7j\.5' && echo "$closed" | grep -q 'a7j\.6'
}

for i in $(seq 1 "$MAX"); do
  if is_done; then echo "[driver] US4-US6 all closed — DONE at increment $i $(date)" | tee -a "$LOG"; break; fi
  before="$(git rev-parse --short HEAD)"
  echo "[driver] === increment $i/$MAX $(date +%H:%M:%S) (HEAD $before) ===" | tee -a "$LOG"

  # Fresh-context process. Skip-permissions so it runs unattended; one increment then exits.
  claude -p "$PROMPT" --dangerously-skip-permissions >>"$LOG" 2>&1
  rc=$?

  after="$(git rev-parse --short HEAD)"
  echo "[driver] increment $i rc=$rc HEAD $before -> $after $(date +%H:%M:%S)" | tee -a "$LOG"
  if [ "$before" = "$after" ] && [ $rc -ne 0 ]; then
    echo "[driver] no commit + nonzero exit — stopping to avoid a spin. Inspect $LOG." | tee -a "$LOG"; exit 1
  fi
done
echo "[driver] finished $(date)" | tee -a "$LOG"
