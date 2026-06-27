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

SYS='You are a senior code reviewer. Review the provided Go files for bugs, security issues, and code-quality problems. Output ONLY a single JSON object — no prose, no markdown fences — matching exactly this schema: {"summary": string, "findings": [{"file": string, "line": number|null, "severity": "info"|"warning"|"error", "title": string, "detail": string}]}'

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
