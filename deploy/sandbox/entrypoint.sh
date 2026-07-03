#!/bin/sh
# entrypoint.sh — runs sst/opencode non-interactively over the checkout and writes
# ONLY the JSON findings object to /out/review.json.
#
# Why sst/opencode (not opencode-ai/opencode): the latter is ARCHIVED at v0.0.55
# with a FROZEN model registry, so current OpenRouter slugs 404 and it falls back
# to a dead default model. sst/opencode has a built-in OpenRouter provider with no
# frozen registry → any current model the provider serves works (manyforge-ht8).
#
# Environment (injected by the sandbox runner — internal/agents/coding/service.go):
#   LLM_API_KEY   — provider API key (forwarded only to the allowlisted LLM host)
#   LLM_BASE_URL  — provider base URL (used only to derive the egress-allowlist host;
#                   opencode's built-in provider already knows its endpoint)
#   LLM_MODEL     — model slug, e.g. "google/gemini-2.5-pro" or "claude-3-5-sonnet"
#   LLM_PROVIDER  — opencode provider id: one of openrouter|anthropic|openai
set -eu

mkdir -p /out

# Writable dirs on the read-only root fs all live under the /tmp tmpfs. opencode
# mkdir -p's its data/config/state dirs at startup, so they MUST be writable;
# pre-create them. There are NO OPENCODE_DATA/CACHE/STATE_DIR env vars — opencode
# resolves these via the XDG base-dir vars, so we point those at /tmp.
export HOME=/tmp
export XDG_DATA_HOME=/tmp/.local/share
export XDG_CACHE_HOME=/tmp/.cache
export XDG_CONFIG_HOME=/tmp/.config
export XDG_STATE_HOME=/tmp/.local/state
mkdir -p "$XDG_DATA_HOME" "$XDG_CACHE_HOME" "$XDG_CONFIG_HOME" "$XDG_STATE_HOME"

# Kill every non-LLM egress so the ONLY runtime network host is the OpenRouter API
# (which the sandbox egress proxy allowlists). The OpenRouter provider SDK is
# compiled into the binary (no npm fetch). These disable the models.dev catalog
# fetch and the GitHub/opencode.ai self-update check respectively.
export OPENCODE_DISABLE_MODELS_FETCH=1
export OPENCODE_DISABLE_AUTOUPDATE=1
export OPENCODE_DISABLE_PRUNE=1

# Copy the read-only checkout into the writable tmpfs and review it there
# (opencode's read tools are cwd-scoped; both the root fs and the /work mount are
# read-only). cp -r (NOT -a): --cap-drop ALL removes CAP_CHOWN, so preserving
# ownership fails; -r copies content+perms without chown.
cp -r /work /tmp/src
cd /tmp/src

# Provider selects the opencode built-in SDK (model prefix + auth.json key). Only
# these three are validated/supported via the sandbox; ollama/vllm use the host-side
# direct-API path and never reach here.
case "${LLM_PROVIDER:-}" in
  openrouter|anthropic|openai) : ;;
  *) echo "entrypoint: unsupported LLM_PROVIDER='${LLM_PROVIDER:-}'" >&2; exit 2 ;;
esac

# Model id for a built-in provider is "<provider>/<slug>"; the slug itself may
# contain a slash (e.g. openrouter/google/gemini-2.5-pro).
MODEL="${LLM_PROVIDER}/${LLM_MODEL}"

# Credentials: write opencode's auth.json (the file the interactive `/connect`
# flow produces) so the OpenRouter API key is attached as the Authorization
# header. This is the ONLY key path that works with the models.dev catalog
# disabled — the config `options.apiKey` and bare-env auto-detect both depend on
# the catalog entry and send NO auth header without it (verified empirically,
# manyforge-ht8). The key lands only in the /tmp tmpfs (ephemeral, OUTSIDE the
# reviewed cwd /tmp/src); egress is locked to the LLM host so it can't be
# exfiltrated, and bash/webfetch are denied. The provider is a validated enum value
# and the supported providers' keys are [A-Za-z0-9-] (no JSON metacharacters), so
# direct interpolation is safe.
mkdir -p "$XDG_DATA_HOME/opencode"
printf '{"%s":{"type":"api","key":"%s"}}\n' "$LLM_PROVIDER" "$LLM_API_KEY" > "$XDG_DATA_HOME/opencode/auth.json"

# Write the config OUTSIDE the reviewed cwd (/tmp, not /tmp/src). It sets the
# default model and a read-only permission profile: deny every mutation/tool,
# allow only read/grep/glob (never "ask" — headless run has no TTY to answer a
# prompt). Credentials come from auth.json above, not from here.
# The custom OpenRouter slug is absent from the (disabled) models.dev catalog, so opencode
# has no output-token limit for it and falls back to a small default. A reasoning model
# (glm-5.2 burns ~9k reasoning tokens/lane) then exhausts that budget mid-answer and the final
# JSON is truncated → ParseFindings fails (manyforge-6h1). Declare a generous per-model
# options.max_tokens so reasoning + the findings JSON both fit. options is passed straight to
# the provider SDK (OpenRouter → request max_tokens). The model key is the slug WITHOUT the
# provider prefix ($LLM_MODEL); provider + slug are validated inputs with no JSON metacharacters.
export OPENCODE_CONFIG=/tmp/opencode.json
printf '%s\n' '{
  "$schema": "https://opencode.ai/config.json",
  "model": "'"${MODEL}"'",
  "provider": {
    "'"${LLM_PROVIDER}"'": {
      "models": {
        "'"${LLM_MODEL}"'": {
          "options": { "max_tokens": 32000 }
        }
      }
    }
  },
  "permission": {
    "read": "allow",
    "glob": "allow",
    "grep": "allow",
    "edit": "deny",
    "bash": "deny",
    "webfetch": "deny",
    "websearch": "deny",
    "task": "deny",
    "external_directory": "deny"
  }
}' > "$OPENCODE_CONFIG"

# Scope the review to the PR's changed files when the runner supplied the list
# (/out/review_files.txt, one repo-relative path per line). This keeps findings on
# the diff so they can be posted as inline comments. Absent/empty → whole-repo review.
# Review instructions come from the HOST at runtime (/out/review_instructions.txt, written
# by service.go from the shared reviewInstructions constant) so the SAME prompt drives local
# and cloud reviews and a prompt change needs no image rebuild. The baked default below is
# only the fallback when the host did not provide one; keep it in sync with localreview.go
# reviewInstructions / tools/local-model-eval/run.sh.
if [ -s /out/review_instructions.txt ]; then
  INSTRUCTIONS=$(cat /out/review_instructions.txt)
else
INSTRUCTIONS='You are a senior software engineer reviewing a pull request. Surface every plausible correctness, security, or robustness concern — including ones you are only moderately confident about — and express your confidence through the severity field rather than by staying silent. Do not withhold a real risk because it seems minor or uncertain. Still skip pure style/formatting preferences, and do not fabricate issues with no basis in the code.

Prioritize in this order: (1) bugs and correctness errors (crashes, nil/undefined access, logic errors, race conditions, incorrect results); (2) security vulnerabilities (injection, auth/authorization gaps, secret exposure, unsafe or unbounded input); (3) robustness and maintainability problems (unhandled errors, resource leaks, missing validation, silent failures).

Set the severity of each finding to exactly one of:
- "error": a real bug or security vulnerability causing incorrect behavior, a crash, data loss, or an exploitable condition.
- "warning": a likely problem or risky pattern that should be fixed (e.g. an unhandled error, a missing bound/validation, a resource leak).
- "info": a plausible concern or worthwhile improvement worth surfacing to the reviewer — when unsure whether something is a real issue, prefer flagging it here rather than omitting it (but never pure style).

You are given the changed code as unified-diff hunks: each block is headed by "=== <path> ===", and every changed line shows its current-file line number in the left gutter with a +/space marker. Use that real file path and gutter line number in each finding. Report each distinct issue once. Only return an empty findings array if the diff genuinely contains nothing worth surfacing.'
fi

SCOPE='Review the code in the current project.'
if [ -s /out/review_diff.txt ]; then
  DIFF=$(cat /out/review_diff.txt)
  SCOPE="Review ONLY the following changed hunks from this pull request. Each block is headed by '=== <path> ===' and shows the changed lines with their current-file line numbers in the gutter. Cite those gutter line numbers. You MAY open the full files in the project for additional context.

${DIFF}"
elif [ -s /out/review_files.txt ]; then
  FILES=$(tr '\n' ' ' < /out/review_files.txt)
  SCOPE="Review ONLY these files changed in this pull request (paths are relative to the project root): ${FILES}
Focus on the changed code; do not report issues in files outside this list."
fi
PROMPT="${INSTRUCTIONS}
${SCOPE}
Output ONLY a single JSON object to stdout — no prose, no markdown fences, no explanation —
matching exactly this schema:
{\"summary\": string, \"findings\": [{\"file\": string, \"line\": number|null, \"severity\": \"info\"|\"warning\"|\"error\", \"title\": string, \"detail\": string}]}"

# `opencode run` executes a single prompt headlessly (no TUI) and prints the
# assistant's final text to stdout → review.json. -m pins the model
# (belt-and-suspenders with the config default). NO_COLOR strips ANSI escapes.
# Capture opencode's exit code so usage capture below can't mask a failed review.
set +e
NO_COLOR=1 opencode run -m "$MODEL" "$PROMPT" > /out/review.json 2> /out/stderr.log
rc=$?
set -e

# Capture token usage for cost accounting. opencode persists a session row with
# running token totals to a SQLite DB under XDG_DATA_HOME; opencode's own cost is 0
# for a custom OpenRouter slug, so the host prices it from these counts. Best-effort:
# any failure leaves /out/usage.json absent and the host records 0.
DB=$(ls -t "$XDG_DATA_HOME"/opencode/opencode*.db 2>/dev/null | head -1)
if [ -n "$DB" ]; then
  # opencode (sst) records per-session usage AND its own computed cost (which correctly
  # prices cache-read tokens — the dominant category, since the agentic loop re-reads the
  # cached context every turn). Sum across sessions (a run may spawn sub-agent sessions)
  # and hand the host both the cost and the full token breakdown. The host uses `cost`
  # directly when >0 and only falls back to catalog pricing for a custom slug opencode
  # couldn't price (cost=0). Cache-read/write are reported so token accounting is honest.
  sqlite3 -json "$DB" \
    "SELECT COALESCE(SUM(cost),0) AS cost, COALESCE(SUM(tokens_input),0) AS input, COALESCE(SUM(tokens_output),0) AS output, COALESCE(SUM(tokens_reasoning),0) AS reasoning, COALESCE(SUM(tokens_cache_read),0) AS cache_read, COALESCE(SUM(tokens_cache_write),0) AS cache_write FROM session;" \
    > /out/usage.json 2>/dev/null || true
fi

exit "$rc"
