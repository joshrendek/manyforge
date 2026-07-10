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
#   LLM_BASE_URL  — provider base URL. For openrouter/anthropic/openai this is used only
#                   to derive the egress-allowlist host (opencode's built-in provider
#                   already knows its endpoint). For vllm/ollama/huggingface it IS the
#                   provider base — passed straight through as the bundled
#                   openai-compatible provider's options.baseURL (e.g. http://host:1234/v1,
#                   or https://router.huggingface.co/v1).
#   LLM_MODEL     — model slug, e.g. "google/gemini-2.5-pro", "claude-3-5-sonnet", or
#                   "zai-org/GLM-5.2:fireworks-ai" (the HF router pins the partner with ":").
#   LLM_PROVIDER  — one of openrouter|anthropic|openai|vllm|ollama|huggingface. The first
#                   three use opencode's BUILT-IN SDK providers; the rest are OpenAI-compatible
#                   endpoints with no built-in provider, and map to a CUSTOM opencode provider
#                   ("local", @ai-sdk/openai-compatible) below — NOT the built-in openai
#                   provider, which speaks the Responses API. See LLM_OPENCODE_MODE.
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

# Provider gate. LLM_OPENCODE_MODE selects WHICH OPENCODE MECHANISM serves the model, and
# nothing else:
#   builtin — opencode's built-in SDK provider (model prefix + auth.json key).
#   compat  — the bundled @ai-sdk/openai-compatible provider, for any OpenAI-compatible
#             /v1/chat/completions endpoint opencode has no built-in provider for.
#
# This deliberately does NOT encode network trust or model capability, which are separate
# axes handled elsewhere (manyforge-bhx):
#   trust      → the credential's allow_private_base_url flag (netsafe dial policy).
#   capability → isConstrainedProvider() in internal/agents/coding/reviewpayload.go.
# It used to be a single LLM_LOCAL=0|1 flag, which worked only while "uses the compat
# provider" and "is a private on-host server serving a small model" happened to coincide.
# huggingface broke that: opencode has no built-in provider for the HF router (its models.dev
# catalog is disabled here), so it needs the compat mechanism — yet it is a public gateway
# serving frontier-class models, so it is neither private nor constrained.
case "${LLM_PROVIDER:-}" in
  openrouter|anthropic|openai)  LLM_OPENCODE_MODE=builtin ;;
  vllm|ollama|huggingface)      LLM_OPENCODE_MODE=compat ;;
  *) echo "entrypoint: unsupported LLM_PROVIDER='${LLM_PROVIDER:-}'" >&2; exit 2 ;;
esac

# SECURITY: the provider config/auth.json below interpolate these connector-supplied
# values into JSON string literals and keys. A value containing a JSON metacharacter
# (" or \) could break out of its string and inject config keys — e.g. overriding the
# read-only "permission" block (pins MF-KUBE-SANDBOX-19/20/21). Legitimate base URLs,
# model slugs, and API keys never contain these; reject any that do.
for _mfval in "${LLM_BASE_URL:-}" "${LLM_MODEL:-}" "${LLM_API_KEY:-}"; do
  case "$_mfval" in
    *'"'*|*'\'*) echo "entrypoint: LLM_* value contains a JSON metacharacter" >&2; exit 2 ;;
  esac
done

if [ "$LLM_OPENCODE_MODE" = compat ]; then
  # An OpenAI-compatible /v1/chat/completions server: vLLM/Ollama/LM Studio on-host, or the
  # HuggingFace Inference Providers router over the public internet. Use the bundled
  # @ai-sdk/openai-compatible provider (Chat Completions) — NOT the built-in openai
  # provider, which speaks the Responses API (/v1/responses) that these servers don't
  # serve. Verified: opencode loads this provider offline (no npm). LLM_BASE_URL is the
  # server's OpenAI base (e.g. http://host:1234/v1, or https://router.huggingface.co/v1).
  #
  # "local" here is OPENCODE'S provider id, not a claim about where the server runs; it must
  # match the auth.json key and the MODEL prefix below. Renaming it would churn the
  # manyforge-9er pin for no behavioral gain.
  MODEL="local/${LLM_MODEL}"
  mkdir -p "$XDG_DATA_HOME/opencode"
  printf '{"local":{"type":"api","key":"%s"}}\n' "$LLM_API_KEY" > "$XDG_DATA_HOME/opencode/auth.json"
  # small_model pins opencode's auxiliary model (session title/summary generation) to the
  # same review model+key. Without it opencode defaults small_model to Claude Haiku and bills
  # a throwaway title call to this provider/key on every run (manyforge discards the title —
  # the check-run title is hardcoded). See manyforge-qxe.
  # Output-token budget. 8192 is tuned for the on-host small models (Ollama/vLLM), some of
  # which reject a max_tokens above their configured context. huggingface reaches the HF
  # router, whose frontier models are the SAME reasoning models the built-in branch already
  # budgets 32000 for: glm-5.2 burns ~9k reasoning tokens before it emits a character, so an
  # 8192 cap truncates the findings JSON mid-answer and ParseFindings fails (manyforge-6h1).
  # Do not collapse these two numbers — they are tuned for different classes of model.
  case "$LLM_PROVIDER" in
    huggingface) COMPAT_MAX_TOKENS=32000 ;;
    *)           COMPAT_MAX_TOKENS=8192 ;;
  esac
  export OPENCODE_CONFIG=/tmp/opencode.json
  printf '%s\n' '{
  "$schema": "https://opencode.ai/config.json",
  "model": "'"${MODEL}"'",
  "small_model": "'"${MODEL}"'",
  "provider": {
    "local": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Local",
      "options": { "baseURL": "'"${LLM_BASE_URL}"'" },
      "models": { "'"${LLM_MODEL}"'": { "options": { "max_tokens": '"${COMPAT_MAX_TOKENS}"' } } }
    }
  },
  "permission": {
    "read": "allow", "glob": "allow", "grep": "allow",
    "edit": "deny", "bash": "deny", "webfetch": "deny", "websearch": "deny",
    "task": "deny", "external_directory": "deny"
  }
}' > "$OPENCODE_CONFIG"
else
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
#
# manyforge-1s9: GLM has no first-class prompt caching via OpenRouter — opencode only emits
# Anthropic-style cache_control breakpoints for anthropic/claude models (transform.ts gate),
# and every GLM upstream on OpenRouter reports implicit-caching=false. To give the agentic
# loop a chance to reuse its growing prefix instead of re-billing the full context each turn,
# pin routing to z.ai's own backend — the one upstream that does implicit prefix caching per
# z.ai's docs — and keep every turn on it (allow_fallbacks:false). Best-effort: it helps only
# if z.ai caches implicitly. Scoped to z-ai/GLM slugs so any other OpenRouter model is
# unaffected; a non-GLM custom slug keeps the plain options. The routing preference is passed
# via opencode's documented provider.<id>.models.<slug>.options.provider passthrough.
MODEL_OPTIONS='"max_tokens": 32000'
case "${LLM_PROVIDER}/${LLM_MODEL}" in
  openrouter/z-ai/*|openrouter/*glm*)
    MODEL_OPTIONS=${MODEL_OPTIONS}', "provider": { "order": ["z-ai"], "allow_fallbacks": false }' ;;
esac
# small_model pins opencode's auxiliary model (title/summary generation) to the same review
# model+key. Without it opencode defaults small_model to Claude Haiku and bills a throwaway
# title call to this provider/key on every run (manyforge discards the title). See manyforge-qxe.
export OPENCODE_CONFIG=/tmp/opencode.json
printf '%s\n' '{
  "$schema": "https://opencode.ai/config.json",
  "model": "'"${MODEL}"'",
  "small_model": "'"${MODEL}"'",
  "provider": {
    "'"${LLM_PROVIDER}"'": {
      "models": {
        "'"${LLM_MODEL}"'": {
          "options": { '"${MODEL_OPTIONS}"' }
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
fi

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
# stderr is LEFT ON the container's stderr (NOT redirected to a file) so the host runner
# receives opencode's live tool-call narration ("Read …/Grep …") and can stream it into the
# review progress heartbeat. stdout still goes to review.json — the two fds are independent, so
# review.json stays clean. The host reads the full stderr from SandboxResult.Stderr for the
# failure tail. Capture opencode's exit code so usage capture below can't mask a failed review.
set +e
NO_COLOR=1 opencode run -m "$MODEL" "$PROMPT" > /out/review.json
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

# KubeRunner (Task 4.4): k8s pod logs merge stdout+stderr with no stream tags, so
# review.json/usage.json can't just be left on stdout like DockerRunner's shared
# host /out mount lets them be — the host instead extracts them from the log
# stream via nonce-scoped markers. MF_MARKER_NONCE is a fresh random value the
# KubeRunner sets per-run (never on DockerRunner, so this block is a no-op there);
# gating on it keeps the DOCKER path byte-for-byte unaffected. Base64-encode each
# file to a single line (no embedded newlines/metacharacters that could be
# mistaken for a marker) — this also defeats a prompt-injected PR that tries to
# forge these markers itself: it would need to guess this run's random nonce.
# Narration continues to go to stderr; this block is stdout only.
if [ -n "${MF_MARKER_NONCE:-}" ]; then
  printf '===MF-REVIEW-%s-BEGIN===\n' "$MF_MARKER_NONCE"
  base64 -w0 /out/review.json 2>/dev/null || true
  printf '\n===MF-REVIEW-%s-END===\n' "$MF_MARKER_NONCE"
  printf '===MF-USAGE-%s-BEGIN===\n' "$MF_MARKER_NONCE"
  base64 -w0 /out/usage.json 2>/dev/null || true
  printf '\n===MF-USAGE-%s-END===\n' "$MF_MARKER_NONCE"
fi

exit "$rc"
