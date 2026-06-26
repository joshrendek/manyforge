#!/bin/sh
# entrypoint.sh — runs opencode-ai/opencode v0.0.55 non-interactively over the
# checkout and writes ONLY the JSON findings object to /out/review.json.
#
# Environment (injected by the sandbox runner — internal/agents/coding/service.go):
#   LLM_API_KEY   — provider API key (forwarded only to the allowlisted LLM host)
#   LLM_BASE_URL  — provider base URL (e.g. https://openrouter.ai/api/v1)
#   LLM_MODEL     — model id (e.g. "auto" for OpenRouter's auto-router)
set -eu

mkdir -p /out
export HOME=/tmp

# opencode-ai v0.0.55 creates its `.opencode` data dir in the CURRENT WORKING DIR
# and needs it writable; the checkout is mounted READ-ONLY at /work and the root fs
# is read-only too. Copy the checkout into the writable tmpfs and run opencode there
# so it can both read the code (its tools are cwd-scoped) and create `.opencode`.
# Use `cp -r` (NOT `cp -a`): --cap-drop ALL removes CAP_CHOWN, so preserving
# ownership fails; -r copies content+perms without chown.
cp -r /work /tmp/src
cd /tmp/src

# Map ManyForge's generic LLM_* contract onto opencode-ai's config schema, which is
# provider-scoped: models are "<provider>.<model>" and the key lives in
# providers.<provider>.apiKey (plus the matching *_API_KEY env). MVP targets
# OpenRouter (the dogfood provider). TODO(manyforge-2nd): generalize to
# anthropic/openai by inspecting LLM_BASE_URL.
# The provider API key is passed ONLY via the env var (opencode-ai reads
# OPENROUTER_API_KEY) — it is NOT written into the on-disk config, so the reviewed
# code (which opencode reads from the cwd) can't contain the key. (Egress is still
# restricted to the LLM host, so even a prompt-injected read of the env can't
# exfiltrate it; redacting the key from review output is a tracked hardening.)
export OPENROUTER_API_KEY="$LLM_API_KEY"

# opencode-ai v0.0.55 does NOT read OPENCODE_CONFIG; Viper loads ".opencode.json"
# from fixed paths ($HOME, $XDG_CONFIG_HOME/opencode, the cwd). Write it ONLY to
# $HOME (= /tmp, OUTSIDE the reviewed cwd /tmp/src) so the config never lands inside
# the directory opencode reviews. (Without a found config, opencode silently uses its
# DEFAULT model instead of the configured one.)
printf '%s\n' '{
  "data": { "directory": "/tmp/.opencode" },
  "providers": { "openrouter": { "disabled": false } },
  "agents": {
    "coder":  { "model": "openrouter.'"${LLM_MODEL}"'", "maxTokens": 4000 },
    "task":   { "model": "openrouter.'"${LLM_MODEL}"'", "maxTokens": 4000 },
    "title":  { "model": "openrouter.'"${LLM_MODEL}"'", "maxTokens": 80 }
  }
}' > "$HOME/.opencode.json"

PROMPT='Review all code in the current project for bugs, security issues, and code-quality problems.
Output ONLY a single JSON object to stdout — no prose, no markdown fences, no explanation —
matching exactly this schema:
{"summary": string, "findings": [{"file": string, "line": number|null, "severity": "info"|"warning"|"error", "title": string, "detail": string}]}'

# opencode-ai v0.0.55: `-p` non-interactive prompt, `-q` no spinner, `-f text` so the
# model's final message (the JSON object) lands in review.json (NOT opencode's own
# json envelope).
opencode -p "$PROMPT" -q -f text > /out/review.json 2> /out/stderr.log
