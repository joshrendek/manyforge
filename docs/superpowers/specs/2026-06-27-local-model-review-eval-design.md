# Local-model code-review eval — design

**Date:** 2026-06-27
**Status:** approved (Phase A of "local Ollama code review"; Phase B = sandbox integration, separate brainstorm)

## Goal

Determine which small local model can drive opencode's review loop and emit
parseable, useful findings — fast enough for the 5-min sandbox budget — so we pick
a winner before doing the harder sandbox→host-Ollama integration.

## Why this shape

opencode is an *agent*: it needs the model to drive a tool loop (Glob/Read) and
then emit strict JSON. Small local models are often weak at tool-calling, so the
eval must exercise the **real opencode path** (Approach A), not a one-shot prompt —
that's the only method that predicts integration success.

## Candidates

`qwen2.5-coder:7b`, `qwen2.5-coder:14b`, `gemma3:12b`, `qwen2.5-coder:32b` (newest
available tags). 64 GB Apple-Silicon host runs all comfortably via native Ollama
(Metal). gemma3 is the general-vs-coder comparison point.

## Components

1. **Ollama setup** — native install (`brew install ollama`), `ollama serve`
   (host `localhost:11434`), `ollama pull` the four models.
2. **Fixture** — `tools/local-model-eval/fixture/`: a small Go package with ~4
   planted issues, each with a known expected finding:
   - nil-pointer dereference
   - ignored error return
   - hardcoded credential
   - unbounded/unsafe input
   Plus 1–2 clean files as noise.
3. **opencode→Ollama config** — opencode pointed at the local Ollama endpoint
   (openai-compatible at `localhost:11434/v1`, or opencode's built-in `ollama`
   provider — resolved empirically; small-model tool-calling is make-or-break).
4. **Eval harness** — `tools/local-model-eval/run.sh` + a tiny Go scorer:
   - For each model: `opencode run -m <ollama model> "<PROMPT>"` in the fixture dir,
     using the **exact review prompt + JSON schema from
     `deploy/sandbox/entrypoint.sh`**. Capture stdout + wall-clock.
   - Pipe output through the **real `coding.ParseFindings`** → record: parses?,
     n_findings, planted-issues-caught (X/4), latency, raw snippet on failure.
5. **Report** — `tools/local-model-eval/results.md`: model | parses? | issues
   caught | latency | notes.

## Data flow

fixture → opencode (drives Ollama tool loop) → review.json → ParseFindings → score.

## Error handling

A model that can't drive opencode's tool loop or won't emit JSON is recorded as a
failed result (with a raw-output snippet) — a valid eval outcome (model not viable),
not a harness error.

## Out of scope (Phase B)

Sandbox→host-Ollama networking (the `--internal` network + SSRF egress proxy
refuses private IPs), a per-provider entrypoint, and UI/agent wiring. This eval
runs opencode on the host directly against native Ollama — no manyforge sandbox
changes.

## Success criteria

At least one candidate parses reliably and catches ≥3/4 planted issues within the
5-min budget. The report ranks all four so we can pick the smallest viable model
for Phase B.

## Results & conclusions (2026-06-27)

**All four models catch 4/4 planted issues** with correct severities via the
direct Ollama `/v1` path (files inline), in 15–40s. Recommended: **`qwen2.5-coder:14b`**
(precise — exactly the 4 findings — 23s); `qwen2.5-coder:7b` for raw speed (15s,
slightly over-reports); `gemma3:12b` a strong general-model alternative (22s);
`qwen2.5-coder:32b` gives no quality gain over 14b and is slower. Full table:
`tools/local-model-eval/results.md`.

**Critical finding — opencode is unusable for small local models.** Via
`opencode run`, every candidate failed: they emit a `task` tool-call as their
"answer" (when the delegation tool is allowed), or hang/time out (when it's
denied) — even with `--pure`, a clean HOME, and the sandbox's permission profile.
The same models, handed the files directly via Ollama's `/v1` chat API, review
correctly in ~20s. opencode's large agent context + tool-calling protocol is too
much for these models.

**Phase B implication (drives the next brainstorm):** the code-review sandbox
currently runs *everything* through opencode. For **local providers** that won't
work — Phase B must add a **direct-API review path** (fetch the changed files,
send them inline to the provider's chat endpoint, parse the JSON) as an
alternative to opencode, selected by provider. This is a real fork in the sandbox
architecture, separate from the networking question (sandbox→host Ollama).

**Parser improvement landed during the eval** (`internal/agents/coding/findings.go`):
`ParseFindings` now normalizes severity (trim + lowercase) — 7b emitted
`" warning"` and 32b `"high"`. This also hardens the cloud path.
