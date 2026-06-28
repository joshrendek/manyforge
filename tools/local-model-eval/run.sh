#!/usr/bin/env bash
# Local-model code-review eval (Phase A). For each Ollama model, run a code review
# of fixture/ (4 planted issues) and score the output with the REAL
# coding.ParseFindings, then emit results.md.
#
# IMPORTANT — why direct-API, not opencode: small local models cannot drive
# opencode's agent loop. Via `opencode run` they either delegate (emit a `task`
# tool-call as their answer), hang, or time out — even with --pure + a clean HOME +
# task denied. The SAME models, handed the files directly via Ollama's /v1
# chat-completions API, review correctly in ~20s. So this harness uses the
# direct-API path; that is also the path Phase B (sandbox integration) must take
# for local providers rather than opencode. See results.md.
#
# Usage: ./run.sh [ "model1 model2 ..." ]
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
FIXTURE="$HERE/fixture"
REPO="$(cd "$HERE/../.." && pwd)"
OLLAMA_URL="${OLLAMA_URL:-http://localhost:11434}"
MODELS="${1:-qwen2.5-coder:7b qwen2.5-coder:14b gemma3:12b qwen2.5-coder:32b}"

# Balanced review instructions — kept in sync with deploy/sandbox/entrypoint.sh.
# NOTE: this harness sends whole-file fixtures, not rendered hunks; the prompt text
# is kept in sync with localreview.go/entrypoint.sh but the input format differs.
SYS='You are a senior software engineer reviewing a pull request. Report only genuine problems you are confident about — do NOT invent issues, speculate, or flag pure style/formatting preferences.

Prioritize in this order: (1) bugs and correctness errors (crashes, nil/undefined access, logic errors, race conditions, incorrect results); (2) security vulnerabilities (injection, auth/authorization gaps, secret exposure, unsafe or unbounded input); (3) notable maintainability problems (unhandled errors, resource leaks, missing validation). Skip cosmetic style and formatting.

Set each finding severity to exactly one of:
- "error": a real bug or security vulnerability causing incorrect behavior, a crash, data loss, or an exploitable condition.
- "warning": a likely problem or risky pattern that should be fixed (e.g. an unhandled error, a missing bound/validation, a resource leak).
- "info": a minor but worthwhile maintainability suggestion (never pure style).

You are given the changed code as unified-diff hunks: each block is headed by "=== <path> ===", and every changed line shows its current-file line number in the left gutter with a +/space marker. Use that real file path and gutter line number in each finding. Report each distinct issue once. If there are no genuine problems, return an empty findings array. Review the provided Go file(s) and output ONLY a single JSON object — no prose, no markdown fences — matching exactly this schema: {"summary": string, "findings": [{"file": string, "line": number|null, "severity": "info"|"warning"|"error", "title": string, "detail": string}]}'

SCORER=/tmp/eval-scorer
echo "building scorer…"
( cd "$REPO" && go build -o "$SCORER" ./tools/local-model-eval/scorer ) || { echo "scorer build failed"; exit 1; }

# Build the review request body once (files inline).
USER=$(printf 'Files to review:\n\n=== service.go ===\n%s\n\n=== util.go ===\n%s' \
  "$(cat "$FIXTURE/service.go")" "$(cat "$FIXTURE/util.go")")

RESULTS="$HERE/results.md"
{
  echo "# Local-model code-review eval — results"
  echo
  echo "Fixture: 4 planted issues (nil-deref, ignored-error, hardcoded-secret, unbounded-input) + 1 clean file."
  echo "Method: direct Ollama /v1 review with files inline (opencode's agent loop is unusable for small local models — see run.sh header)."
  echo "Scored by the real coding.ParseFindings. Host: native Ollama (Metal)."
  echo
  echo "| Model | Parses? | Issues caught | Latency | Findings | Notes |"
  echo "|-------|---------|---------------|---------|----------|-------|"
} > "$RESULTS"

for M in $MODELS; do
  echo "=== $M ==="
  SAFE="$(echo "$M" | tr '/:' '__')"
  OUT="/tmp/eval-out-$SAFE.txt"
  start=$(date +%s)
  jq -n --arg m "$M" --arg s "$SYS" --arg u "$USER" \
    '{model:$m, messages:[{role:"system",content:$s},{role:"user",content:$u}], stream:false, options:{temperature:0, num_ctx:8192}}' \
    | curl -s -m 300 "$OLLAMA_URL/v1/chat/completions" -d @- \
    | jq -r '.choices[0].message.content // .error.message // "NO CONTENT"' > "$OUT" 2>/dev/null
  secs=$(( $(date +%s) - start ))

  score="$("$SCORER" < "$OUT" 2>&1)"
  echo "$score" | sed 's/^/  /'
  if echo "$score" | grep -q '^PARSE_OK'; then parses=yes; else parses=no; fi
  caught="$(echo "$score" | grep -oE 'CAUGHT [0-9]+' | head -1 | awk '{print $2}')"
  nf="$(echo "$score" | grep -oE 'findings=[0-9]+' | head -1 | cut -d= -f2)"
  note=""; [ "$parses" = "no" ] && note="$(echo "$score" | head -1 | cut -c1-80)"
  echo "| \`$M\` | $parses | ${caught:-0}/4 | ${secs}s | ${nf:-0} | $note |" >> "$RESULTS"
done

echo; echo "=== results ==="; cat "$RESULTS"
