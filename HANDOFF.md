# Handoff — manyforge @ feat/code-review-ui — 2026-06-28 ~13:30 UTC

## ⚠️ Before you clear
- **Uncommitted:** only this `HANDOFF.md` (rolling note) + pre-existing untracked files
  (`.beads/CLAUDE.md`, `.pair/`, screenshots, various `CLAUDE.md`). None is code.
- **Unpushed:** none — `feat/code-review-ui` is up to date with origin (PR #6 updated).
- **Still running (survive this session):** air dev server on **:8081** (`/tmp/mf-air.log`);
  **Ollama** on **:11434** (qwen2.5-coder 7b/14b/32b, gemma3:12b); Docker (Colima):
  `mf-dev` + `mf-egress-proxy`. Sandbox image `manyforge/opencode-sandbox:dev` rebuilt this session.

## State (≤3 sentences)
**Diff-based code review (`manyforge-fqo` #1) is COMPLETE and pushed** on `feat/code-review-ui`
(commits `268fe0d..5c43baf`, PR #6): the agent now sends annotated diff **hunks** (not whole files)
on both the local host-side and cloud opencode paths, from one shared renderer
(`github.ParseHunks`/`RenderAnnotatedHunks`); `ChangedLines`→`ChangedFiles` retains the patch;
64KB budget; skipped(binary)/omitted(over-budget) files surfaced in the review body + audit trail.
Built via subagent-driven development (6 tasks, each spec+quality reviewed); final opus
whole-branch review = **SHIP**; full gate green incl. coding integration; sandbox image rebuilt.

## Resume here
**Live dogfood: DONE locally.** Ran the real path (ChangedFiles→assembleDiffPayload→localReview)
against PR #6's actual 85-file diff with qwen2.5-coder:7b: payload 65383/65536B, 1 skipped
(binary), 65 omitted (surfaced), hunks rendered with correct gutter line numbers, model returned
5 findings, 2 cited in-diff lines → would post as inline comments. (qwen 14b was too slow on 16K
ctx here — >240s; 7b ~140s. For a faster/quality default, prefer 7b locally or raise the timeout.)
**Still NOT done — GitHub-posting dogfood:** the dev DB was RESEEDED (agent `6aeb7a46`/connector
`f5edb238` GONE; agent/repo_connector/business tables EMPTY). To post a real review to PR #6 you
must re-seed (cmd/seeddemo) + create an ollama agent + a repo connector with a real GitHub PAT,
then `POST /api/v1/businesses/{id}/code-reviews {agent_id,repo_connector_id,pr_number:6}`.
NOTE: the host-side github client (netsafe) timed out dialing api.github.com in a `go test` —
`gh`/git work fine; use `gh api .../pulls/6/files` if you need the diff out-of-band.
Next `manyforge-fqo` items: #2 (redact LLM key from review output), #3 (provider generality).

## Run & verify
- Tests (all green as of this handoff): `go test ./internal/agents/coding/... ./internal/connectors/...`;
  `go vet ./...`; `make lint` (golangci 0); `make sec-test`; integration:
  `go test -tags integration -p 1 ./internal/agents/coding/`; drift: `go test -tags contract ./cmd/...`.
- Rebuild sandbox image (after any `entrypoint.sh` change):
  `DOCKER_BUILDKIT=0 docker build --build-arg TARGETARCH=arm64 -f deploy/sandbox/Dockerfile -t manyforge/opencode-sandbox:dev .`
- SDD ledger for this effort: `.superpowers/sdd/progress.md` (prior slice-2 archived alongside).

## Gotchas (don't relearn these)
- **gopls diagnostics lie after edits** — every task this session threw phantom
  "undefined ParseHunks / does not implement / dbgen.* undefined" diagnostics that were STALE.
  `go build`/`go test`/`go vet` is the only truth; verify there, ignore the editor squiggles.
- **`make lint`'s tail can hide the golangci result** — run `golangci-lint run ./...` directly to confirm.
- **The 3 review-prompt copies must stay in sync:** `internal/agents/coding/localreview.go`
  (`reviewInstructions`/`reviewSchemaLine`), `deploy/sandbox/entrypoint.sh`, `tools/local-model-eval/run.sh`.
- **`CreateCodeReviewAgentRun` requires a REAL agent** (INSERT…SELECT FROM agent → no row for a
  foreign/non-existent agent). Integration tests must seed via `seed.agentID`, not `uuid.New()`
  (fixed a pre-existing `ErrNoRows` here in `f821a9a`).

## Decisions & rationale
- **Annotated hunks, not raw diff:** each changed line is rendered with its real new-side gutter
  number so the model's `line` citations land in-diff (become inline comments). Render gutter
  numbers and `commentableLines` both derive from one `ParseHunks` → they always agree.
- **Cloud path = diff + may read files:** opencode gets `/out/review_diff.txt` and is told it MAY
  open full files for context (keeps its agentic strength); local path POSTs the hunks directly.
- **Skip patchless files; cap+truncate over-budget; single call** (multi-call chunking deferred to
  `manyforge-206`). Nothing dropped silently (body note + `agent.coding.review.files_dropped` audit).

## Next steps
1. (Optional) Live-verify on PR #6 with ReviewBot `6aeb7a46`.
2. `manyforge-fqo` #2: redact the LLM key from review output.
3. `manyforge-fqo` #3: generalize `entrypoint.sh` provider mapping beyond OpenRouter.
4. `manyforge-206`: gemini-2.5-pro 504 on large diffs (now smaller via hunks; chunk if still needed).
5. Merge PR #6 when ready (user's call; squash recommended).

## Pointers
- **Spec/plan:** `docs/superpowers/specs/2026-06-28-diff-based-code-review-design.md`,
  `docs/superpowers/plans/2026-06-28-diff-based-code-review.md`.
- **Key files:** `internal/connectors/github/diff.go` (parser+renderer), `internal/connectors/{repo_connector.go,github/client.go}`
  (`ChangedFiles`), `internal/agents/coding/{localreview.go,service.go,review.go}`,
  `deploy/sandbox/entrypoint.sh`.
- **IDs:** Local ReviewBot agent `6aeb7a46` (ollama, qwen2.5-coder:14b); cloud ReviewBot `6c252395`
  (z-ai/glm-5.2); PR #6 (combined slice-2 + diff-based review) → master.
- **bd:** `bd show manyforge-fqo` (item #1 done; #2/#3 open).
