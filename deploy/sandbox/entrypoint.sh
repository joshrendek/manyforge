#!/bin/sh
# entrypoint.sh — runs opencode in non-interactive mode over /work and writes
# ONLY the JSON findings object to /out/review.json.
# Environment (injected by the sandbox runner):
#   LLM_API_KEY   — API key forwarded to the upstream LLM provider
#   LLM_BASE_URL  — OpenAI-compatible base URL (e.g. https://openrouter.ai/api/v1)
#   LLM_MODEL     — model identifier (e.g. anthropic/claude-3-5-sonnet)
set -eu

mkdir -p /out

PROMPT='Review all code in /work for bugs, security issues, and code quality problems.
Output ONLY a single JSON object to stdout — no prose, no markdown, no explanation.
The JSON object must match exactly this schema:
{"summary": string, "findings": [{"file": string, "line": number|null, "severity": "info"|"warning"|"error", "title": string, "detail": string}]}'

# opencode reads provider config (base URL + API key) from opencode.json via
# {env:LLM_API_KEY} and {env:LLM_BASE_URL} substitution; model from {env:LLM_MODEL}.
# The config is placed at /etc/opencode/opencode.json and pointed to by OPENCODE_CONFIG.
opencode run "$PROMPT" > /out/review.json 2> /out/stderr.log
