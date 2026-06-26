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
#   LLM_BASE_URL  — provider base URL (unused: the built-in openrouter provider
#                   already knows its endpoint; kept for the host-allowlist derive)
#   LLM_MODEL     — OpenRouter model slug, e.g. "google/gemini-2.5-pro"
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

# Model id for the built-in OpenRouter provider is "openrouter/<slug>" — the slug
# itself contains a slash (e.g. openrouter/google/gemini-2.5-pro).
MODEL="openrouter/${LLM_MODEL}"

# Credentials: write opencode's auth.json (the file the interactive `/connect`
# flow produces) so the OpenRouter API key is attached as the Authorization
# header. This is the ONLY key path that works with the models.dev catalog
# disabled — the config `options.apiKey` and bare-env auto-detect both depend on
# the catalog entry and send NO auth header without it (verified empirically,
# manyforge-ht8). The key lands only in the /tmp tmpfs (ephemeral, OUTSIDE the
# reviewed cwd /tmp/src); egress is locked to the LLM host so it can't be
# exfiltrated, and bash/webfetch are denied. OpenRouter keys are [A-Za-z0-9-]
# (no JSON metacharacters), so direct interpolation is safe.
mkdir -p "$XDG_DATA_HOME/opencode"
printf '{"openrouter":{"type":"api","key":"%s"}}\n' "$LLM_API_KEY" > "$XDG_DATA_HOME/opencode/auth.json"

# Write the config OUTSIDE the reviewed cwd (/tmp, not /tmp/src). It sets the
# default model and a read-only permission profile: deny every mutation/tool,
# allow only read/grep/glob (never "ask" — headless run has no TTY to answer a
# prompt). Credentials come from auth.json above, not from here.
export OPENCODE_CONFIG=/tmp/opencode.json
printf '%s\n' '{
  "$schema": "https://opencode.ai/config.json",
  "model": "'"${MODEL}"'",
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
SCOPE='Review all code in the current project for bugs, security issues, and code-quality problems.'
if [ -s /out/review_files.txt ]; then
  FILES=$(tr '\n' ' ' < /out/review_files.txt)
  SCOPE="Review ONLY these files changed in this pull request (paths are relative to the project root): ${FILES}
Report each finding's line number from the CURRENT version of the file. Do not report issues in files outside this list."
fi
PROMPT="${SCOPE}
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
  sqlite3 -json "$DB" \
    "SELECT tokens_input AS input, tokens_output AS output, tokens_reasoning AS reasoning FROM session ORDER BY time_created DESC LIMIT 1;" \
    > /out/usage.json 2>/dev/null || true
fi

exit "$rc"
