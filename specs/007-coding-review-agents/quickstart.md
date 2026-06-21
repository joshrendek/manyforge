# Spec 007 — Code Review Agent: End-to-End Quickstart

> **Reference**: [`specs/007-coding-review-agents/spec.md`](spec.md) ·
> [`specs/007-coding-review-agents/contracts/openapi.yaml`](contracts/openapi.yaml)

This guide walks you from a clean checkout to a live GitHub PR review posted by ManyForge.
You need Docker, a running ManyForge server (`make dev`), and a fine-grained GitHub PAT
with **Read access to Contents and Pull requests, Write access to Pull requests** on a
throwaway repository.

---

## 1. Build the two images

```bash
# Egress proxy (allowlists outbound traffic from the sandbox)
docker build -f deploy/egress-proxy/Dockerfile -t manyforge/egress-proxy:dev .

# Sandbox (opencode + git; runs the actual review)
docker build -f deploy/sandbox/Dockerfile -t manyforge/opencode-sandbox:dev .
```

These are the two images the server pulls at runtime.
The four relevant config env vars and their defaults are:

| Env var | Default |
|---|---|
| `MANYFORGE_SANDBOX_IMAGE` | `manyforge/opencode-sandbox:dev` |
| `MANYFORGE_EGRESS_PROXY_IMAGE` | `manyforge/egress-proxy:dev` |
| `MANYFORGE_SANDBOX_EGRESS_ALLOW` | `api.anthropic.com,openrouter.ai,api.openai.com` |
| `MANYFORGE_SANDBOX_WORK_ROOT` | `$HOME/.cache/manyforge/sandbox` (falls back to `/tmp/mf-sandbox`) |

---

## 2. Prerequisites: business ID and auth token

The commands below assume you have already:
- Created an account and business (spec 001 onboarding).
- Obtained a Bearer token (e.g. via `POST /api/v1/auth/login`).

Set shell variables:

```bash
MANYFORGE_URL="http://localhost:8080"
BID="<your-business-uuid>"
TOKEN="<your-bearer-token>"
GITHUB_PAT="<fine-grained-PAT>"
GITHUB_REPO="owner/throwaway-repo"   # must be a real repo you own
PR_NUMBER=1                           # a real open PR on that repo
```

---

## 3. Create an AI credential (BYO provider key)

```bash
curl -s -X POST "$MANYFORGE_URL/api/v1/businesses/$BID/ai_credentials" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "anthropic",
    "api_key": "sk-ant-...",
    "default_model": "claude-opus-4-5",
    "base_url": ""
  }' | tee /tmp/cred.json
```

The response does **not** echo the API key (write-only).

---

## 4. Create a code-review agent

The agent must be linked to the AI credential by setting the same `provider` and `model`.

```bash
curl -s -X POST "$MANYFORGE_URL/api/v1/businesses/$BID/agents" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Code Review Bot",
    "provider": "anthropic",
    "model": "claude-opus-4-5",
    "system_prompt": "You are a meticulous code reviewer. Identify bugs, security issues, and style problems.",
    "allowed_tools": [],
    "autonomy_mode": 0
  }' | tee /tmp/agent.json

AGENT_ID=$(jq -r '.id' /tmp/agent.json)
echo "Agent ID: $AGENT_ID"
```

---

## 5. Create a repo connector

```bash
curl -s -X POST "$MANYFORGE_URL/api/v1/businesses/$BID/repo-connectors" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"type\": \"github\",
    \"display_name\": \"Throwaway Repo\",
    \"base_url\": \"https://api.github.com\",
    \"repo\": \"$GITHUB_REPO\",
    \"api_token\": \"$GITHUB_PAT\"
  }" | tee /tmp/rc.json

RC_ID=$(jq -r '.id' /tmp/rc.json)
echo "Repo connector ID: $RC_ID"
```

The PAT is sealed into the vault immediately. The request shape matches
`connectors.CreateRepoConnectorInput` (`type`, `display_name`, `base_url`, `repo`, `api_token`).

---

## 6. Trigger a code review

```bash
curl -s -X POST "$MANYFORGE_URL/api/v1/businesses/$BID/code-reviews" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"agent_id\": \"$AGENT_ID\",
    \"repo_connector_id\": \"$RC_ID\",
    \"pr_number\": $PR_NUMBER
  }" | tee /tmp/review.json

REVIEW_ID=$(jq -r '.id' /tmp/review.json)
echo "Code review ID: $REVIEW_ID"
echo "Status: $(jq -r '.status' /tmp/review.json)"
echo "Review URL: $(jq -r '.review_url' /tmp/review.json)"
```

Response body: `{"id": "...", "status": "succeeded", "review_url": "https://github.com/owner/repo/pull/1#pullrequestreview-..."}`.
Status is `"succeeded"` on success or `"failed"` on error (the call blocks until the sandbox exits).

The pipeline this triggers (see `service.go`):
1. Resolve repo connector (RLS-scoped)
2. Build GitHub client + resolve AI credential
3. Insert `code_review` row (status = `"pending"`) + audit `agent.coding.review.requested`
4. Fetch PR metadata (head SHA, title, state)
5. Clone PR head into `$MANYFORGE_SANDBOX_WORK_ROOT/<run-id>/checkout` (host-side, credential-free checkout)
6. Audit `agent.coding.opencode.invoked`, run opencode in isolated Docker sandbox:
   - read-only bind-mount of checkout
   - egress allowed **only** to the LLM API host
   - no host env inherited — only `LLM_API_KEY`, `LLM_BASE_URL`, `LLM_MODEL`
   - `--cap-drop ALL`, `--read-only`, `--rm`
7. Parse `/out/review.json` (structured findings)
8. Post one review to the PR via `conn.PostReview` (advisory — no approval gate)
9. `UpdateCodeReviewResult` (status = `"succeeded"`) + audit `agent.coding.review.posted`
10. `os.RemoveAll` the per-run directory

---

## 7. Observe the review on GitHub

Open the PR in your browser or list reviews via the GitHub API:

```bash
curl -s \
  -H "Authorization: Bearer $GITHUB_PAT" \
  -H "Accept: application/vnd.github+json" \
  "https://api.github.com/repos/$GITHUB_REPO/pulls/$PR_NUMBER/reviews"
```

You should see a single review authored by the PAT owner containing the structured findings
rendered as Markdown (summary + findings table).

---

## 8. Fetch the code-review record

```bash
curl -s "$MANYFORGE_URL/api/v1/businesses/$BID/code-reviews/$REVIEW_ID" \
  -H "Authorization: Bearer $TOKEN"
```

Response shape (`CodeReview`):
```json
{
  "ID": "...",
  "Status": "succeeded",
  "Summary": "...",
  "ReviewURL": "",
  "PRNumber": 1
}
```

> Note: `ReviewURL` is populated in the `Trigger` response but not re-fetched in `Get`
> (the `ExternalReviewRef` column holds the numeric review ID; construct the URL from the
> connector config if needed).

---

## 9. Inspect the audit trail

```bash
curl -s "$MANYFORGE_URL/api/v1/businesses/$BID/audit" \
  -H "Authorization: Bearer $TOKEN"
```

You should see four entries for this review (most-recent first):
1. `agent.coding.review.posted` — outputs include `review_url` and findings count
2. `agent.coding.opencode.invoked` — inputs include `image`, `head_sha`, `model`
3. `agent.coding.review.failed` — *only if the run failed*
4. `agent.coding.review.requested` — inputs include `pr` number and `repo_connector_id`

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `status: "failed"` immediately | Clone failed (bad PAT scope / private repo) or PR doesn't exist |
| `status: "failed"` after a delay | opencode exited non-zero; check Docker logs for the sandbox container |
| Empty `review_url` on `GET` | Expected — use the `Trigger` response for the live URL |
| Sandbox timeout | Default wall-clock cap is 10 minutes; raise `MANYFORGE_SANDBOX_TIMEOUT` if needed |
| Docker image not found | Ensure you built both images in step 1 before starting the server |
