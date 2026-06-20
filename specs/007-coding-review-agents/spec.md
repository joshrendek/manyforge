# Feature Specification: Coding & Review Agents (opencode) — Slice 1: Code Review

**Feature Branch**: `007-coding-review-agents`

**Created**: 2026-06-19

**Status**: Draft

**Input**: User description: "Spec 007 — Coding & Review Agents (opencode). First slice: a read-only **code review** agent. Point an agent at a GitHub pull request; it works in an ephemeral, credential-free sandbox; opencode reviews the checkout; the findings are posted back to the PR as a review — automatically, no approval gate. Authoring code / opening PRs, GitLab, webhook auto-trigger, and the Kubernetes sandbox backend are later slices."

## Overview

This is the first slice of spec 007 on the program roadmap (`docs/ROADMAP.md`). It introduces `internal/agents/coding` and the platform's first **sandboxed execution host**, deliberately in its lowest-risk form: a **read-only code review**. A user points an agent at a GitHub pull request; the agent runtime (spec 003) drives a coding tool that provisions an **ephemeral, isolated sandbox with no ambient credentials**, runs **opencode** against a read-only checkout of the PR head, captures structured findings, and posts them back to the pull request as a single review — **automatically, with no approval gate**, because a review comment changes no code.

The slice is scoped to build the genuinely novel and security-critical 007 infrastructure — the ephemeral sandbox, the no-ambient-credentials guarantee, and the opencode invocation contract — in a setting where the repository is never mutated. It establishes the regression contract (sandbox isolation, no-ambient-creds, no-write-capability, opencode invocation, every-action audited) that the later **PR-authoring** slice will extend with write access on top of *proven* sandbox plumbing.

It reuses, without modifying, the spec 003 agent runtime (`Engine`, run loop, effect+autonomy gate, audit) and the spec 004 BYO-credential vault and connector-registration pattern. It adds a **repo connector** (GitHub) that is distinct from the issue-shaped `TicketingConnector`, and a **sandbox runner** with a Docker backend (a Kubernetes-Job backend is deferred behind the same interface). No code authoring, no silent push, and — in this slice — no method on the repo connector that can write code.

## Clarifications

### Session 2026-06-19

- Q: What is the first slice of 007? → A: **Code review, read-only.** A reviewing agent fetches a PR, reasons over a checkout, and posts a review. Authoring code / opening PRs is a deliberately separate, later slice. Rationale: it builds 007's hard, novel, security-critical infrastructure (ephemeral sandbox, no-ambient-creds, opencode invocation contract) where the repository is never mutated.
- Q: Does the review run in a sandbox with opencode, or review the diff with the existing runtime? → A: **Sandbox + opencode now.** opencode runs read-only against a full repo checkout (full-file context, not just the diff). The lean "review the API-fetched diff on the existing runtime" path was rejected because it builds none of 007's actual infrastructure.
- Q: Do reviews / review comments need approval? → A: **No.** A posted review (and any comments within it) is advisory and **executes automatically** — no approval gate. Rationale: 007's gating exists to prevent *mutating the codebase* (no silent push; PRs always gated). A review comment mutates nothing in the repo, so it is auto-executed even under the default `ModeAssist`. The dividing line is "does it change code," not "is it external."
- Q: Where does the sandbox run? → A: **Docker now, Kubernetes later.** A `SandboxRunner` interface with a Docker backend (runs on dev macOS and any Docker host); opencode + git baked into the image. A Kubernetes-Job backend lands later behind the same interface.
- Q: How is opencode wired in? → A: **Native subprocess tool.** A coding tool in the spec 003 tool registry provisions the sandbox, execs opencode headlessly inside it, and captures structured findings — reusing existing tool-gating and audit. This is a deliberate, recorded deviation from the ROADMAP's literal "invoked through the MCP tool host" wording; MCP-wrapping is revisited when PR-authoring needs richer tool access.
- Q: What triggers a review? → A: **Manual / API trigger** ("review PR #N") in this slice — deterministic for the demo and tests, needs no public ingress. GitHub **webhook auto-trigger** (PR opened/synchronized) is a fast follow that reuses spec 004's webhook verification.
- Q: How does the sandbox get the code without holding repo credentials? → A: **Clone on the host; the sandbox is repo-credential-free.** ManyForge fetches the PR head on the host (using the vault credential) into a per-run temp directory and provisions it into the sandbox **read-only**. The sandbox holds only the LLM key (for opencode) and may reach **only** the LLM API endpoint (egress allowlist). The write-capable credential (post review) never leaves the host. This makes the isolation pin trivially true: there is nothing in the sandbox worth stealing.
- Q: What does opencode emit, and how is the review posted? → A: opencode emits **structured findings (JSON)**; ManyForge validates and renders them into **one PR review** (overall summary + a findings list). Inline line-level comments are deferred.
- Q: What is opencode allowed to do during review? → A: A **constrained review posture** — read and reason only; it does not build or run the repository. The sandbox (read-only filesystem, egress allowlist, no credentials) is the backstop if it attempts otherwise.
- Q: GitHub authentication? → A: A **fine-grained Personal Access Token** (`contents:read` + `pull_requests:write` + `metadata:read`), sealed in the vault. A GitHub App (short-lived per-repo installation tokens) is deferred.
- **Program-level decisions adopted** (from `docs/ROADMAP.md` / `.specify/memory/constitution.md`): opencode coding/review agents MUST run in isolated, ephemeral workspaces with no ambient credentials; under Mode 1 their repo *changes* surface as PRs, never silent pushes (this slice produces no repo changes at all).
- **Deferred to the plan** (HOW, not WHAT): concrete table design and `business_id`/`tenant_root_id` scoping + RLS for `repo_connector` and `code_review`; the Docker sandbox spec (image, mount, env allowlist, egress allowlist, wall-clock cap) and the `SandboxRunner` interface; the opencode exec contract and findings JSON schema; the review-posting effect classification; the REST contract (`contracts/openapi.yaml`); the security-regression pins. Captured in `plan-inputs.md`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Review a GitHub pull request on demand (Priority: P1)

An authorized user connects a GitHub repository and asks the platform to review a specific open pull request. The platform runs a coding agent that reads the pull request's code and posts a review back to the pull request — a summary plus specific findings — without any further human action.

**Why this priority**: Turning a pull request into useful, automatically-posted review feedback is the irreducible value of this slice; everything else (isolation, audit) exists to make this safe.

**Independent Test**: Register a GitHub repo connector with a valid token; trigger a review of an existing open PR; confirm a single review appears on that PR containing a summary and findings, and that a `code_review` record is persisted in the requesting business with the run, PR number, and head SHA.

**Acceptance Scenarios**:

1. **Given** a business with a registered GitHub repo connector, **When** a user triggers a review of an open PR, **Then** an agent run executes and a single review is posted to that PR containing an overall summary and a list of findings.
2. **Given** a triggered review, **When** opencode produces findings, **Then** the review is posted **automatically with no approval gate** and the run completes as succeeded.
3. **Given** a PR number that does not exist (or is not visible to the connector), **When** a review is triggered, **Then** the run fails with a typed not-found error and no review is posted; the response does not distinguish "doesn't exist" from "not yours."
4. **Given** a review was posted, **When** the same PR is reviewed again at the same head SHA, **Then** the system records a new `code_review` row (re-review is allowed) and does not corrupt or duplicate the prior record.
5. **Given** opencode returns malformed or empty findings, **When** the run processes them, **Then** the run fails (or posts nothing) deterministically rather than posting a partial/garbage review, and the failure is audited.

---

### User Story 2 - Review runs in a credential-free, isolated sandbox (Priority: P1)

The code review executes inside an ephemeral sandbox that inherits no credentials from the host, cannot read the host filesystem, holds only the single secret it needs (the LLM key), can reach only the LLM API, and is destroyed when the run ends. The repository checkout inside the sandbox is read-only.

**Why this priority**: The repository under review is untrusted input (prompt-injection, or malicious code). Without enforced isolation, a single review could exfiltrate credentials or reach internal systems. This is the security contract that makes US1 safe to run autonomously, and the foundation the PR-authoring slice builds on.

**Independent Test**: Run a review whose sandbox executes a stub that attempts to read host environment variables / git config / SSH keys, write to the checkout, and make a network call to a non-LLM host; confirm each attempt fails (no host secrets present, filesystem read-only, egress blocked) and the sandbox is removed after the run.

**Acceptance Scenarios**:

1. **Given** a review run, **When** the sandbox starts, **Then** its environment contains only the explicitly-injected, run-scoped secrets (the LLM key) and **no** host environment variables, SSH keys, or git credentials.
2. **Given** the sandbox is running, **When** code inside it attempts to write to the repository checkout, **Then** the write fails because the checkout is mounted read-only.
3. **Given** the sandbox is running, **When** code inside it attempts network egress to any host other than the allowlisted LLM API endpoint, **Then** the connection is refused.
4. **Given** a review run completes, times out, or errors, **When** it terminates, **Then** the sandbox and its temporary checkout are destroyed and leave no residual state.
5. **Given** the repo connector for this slice, **When** its interface is inspected, **Then** it exposes no method capable of pushing, committing, or opening a pull request (no-write-capability).

---

### User Story 3 - Every coding action is audited (Priority: P2)

Every step of a review — sandbox provisioning, opencode invocation, and the posted review — is recorded in the append-only audit trail, scoped to the business, attributable to the agent principal, and linked to the run.

**Why this priority**: Auditability is a platform constitution requirement and the only way to trust autonomous, ungated review posting after the fact. It is P2 because US1/US2 deliver and protect the core value; audit makes it accountable.

**Independent Test**: Trigger a review; query the audit trail for the run and confirm entries for workspace provisioning, opencode invocation (inputs: repo + head SHA + prompt identity; outputs: findings summary), and review posting (output: external review reference), each carrying the business, agent principal, and run id.

**Acceptance Scenarios**:

1. **Given** a completed review, **When** the audit trail is queried, **Then** it contains an entry for sandbox provisioning, opencode invocation, and review posting, each scoped to the business and linked to the run.
2. **Given** a review that failed (bad findings, sandbox error), **When** the audit trail is queried, **Then** the failure is recorded with its decision/outcome.
3. **Given** audit entries for a review, **When** an actor without `audit.read` at the business queries them, **Then** they are not returned (tenant isolation).

---

### Edge Cases

- **PR has no changes / is empty**: the review run completes and posts a review stating there is nothing to review (or posts nothing) — deterministically, not an error.
- **Repository is very large / clone is slow**: the run is bounded by a wall-clock cap; on exceeding it the run fails (timeout), the sandbox is destroyed, and the failure is audited; no partial review is posted.
- **LLM endpoint unreachable from the sandbox**: opencode fails; the run fails with a typed provider error; no review posted; failure audited.
- **Token lacks `pull_requests:write`**: posting the review fails with a typed error surfaced to the caller; the run is marked failed; nothing partial is left on the PR.
- **Connector base URL points at a private/internal host** (GitHub Enterprise vs. SSRF): repo connector creation validates the base URL (reuse spec 004's SSRF guard); host-side clone and API calls honor that validation.
- **Re-trigger while a review for the same PR is in flight**: both runs may proceed independently; each persists its own `code_review` row (no shared mutable state). Deduplication is out of scope for this slice.
- **opencode attempts to run repo build/test commands**: blocked by the constrained review posture; if attempted anyway, contained by the read-only filesystem and egress allowlist.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Users with the appropriate permission MUST be able to register a GitHub **repo connector** for a business (base URL, `owner/repo`, credential), with the credential sealed in the spec 004 vault and the row scoped and isolated per business (RLS).
- **FR-002**: The system MUST validate a repo connector's base URL against the SSRF/private-host guard at creation time (reusing spec 004's validation).
- **FR-003**: Users with the appropriate permission MUST be able to trigger a **code review** of a specified open pull request by PR number on a registered repo connector.
- **FR-004**: A triggered review MUST execute as a spec 003 agent run (reusing the existing `Engine`, run lifecycle, and gating), targeting the pull request.
- **FR-005**: The system MUST fetch the pull request metadata and clone the PR head **on the host** using the vault credential; the repository checkout MUST be provisioned into the sandbox **read-only**.
- **FR-006**: The sandbox MUST be **ephemeral** and MUST contain **no ambient credentials** — no host environment variables, SSH keys, or git credentials — only explicitly-injected, run-scoped secrets (the LLM key).
- **FR-007**: The sandbox MUST restrict network egress to an **allowlist** consisting only of the LLM API endpoint; all other egress MUST be refused.
- **FR-008**: The system MUST invoke **opencode** headlessly inside the sandbox in a **constrained review posture** (read and reason; no repository build/execution), against the read-only checkout.
- **FR-009**: opencode MUST emit **structured findings**; the system MUST validate them against a defined schema and reject malformed/empty output deterministically (no partial/garbage review posted).
- **FR-010**: The system MUST render validated findings into a **single pull request review** (overall summary + findings list) and post it back to the pull request **on the host** using the vault credential.
- **FR-011**: Posting the review MUST execute **automatically without an approval gate**, even under the default `ModeAssist` (the review-posting action is classified so it is not queued).
- **FR-012**: The repo connector in this slice MUST expose **no capability to push, commit, or open a pull request** (no-write-capability), enforced at the interface level.
- **FR-013**: The sandbox and its temporary checkout MUST be **destroyed** when the run completes, times out, or errors, leaving no residual state.
- **FR-014**: A review run MUST be bounded by a **wall-clock cap**; exceeding it fails the run, destroys the sandbox, and is audited.
- **FR-015**: The system MUST persist a **`code_review`** record (run, repo connector, PR number, head SHA, status, summary, findings, external review reference, timestamps), scoped and isolated per business (RLS).
- **FR-016**: The system MUST **audit** sandbox provisioning, opencode invocation (inputs and outputs), and review posting, each scoped to the business, attributable to the agent principal, and linked to the run, in the append-only audit trail.
- **FR-017**: Unknown or foreign pull-request / connector identifiers MUST return the same **not-found** shape (no existence oracle), consistent with the platform's authorization model.
- **FR-018**: The `SandboxRunner` MUST be defined as an **interface** with a Docker backend in this slice, so a Kubernetes-Job backend can be added later without changing callers.

### Key Entities *(include if feature involves data)*

- **Repo Connector**: a per-business, RLS-scoped registration of a code-hosting repository (type `github`, base URL, `owner/repo`) whose credential is sealed in the vault. Distinct from the issue-shaped ticketing connector. In this slice it is read-for-clone + write-only-for-review-posting, with no code-write capability.
- **Code Review**: a per-business, RLS-scoped record of one review of one pull request — linked to the agent run, carrying PR number, head SHA, status, the rendered summary, the structured findings, and the external review reference.
- **Agent Run** (reused, spec 003): the bounded execution that performs the review; target type is a pull request.
- **Sandbox** (ephemeral, no table): the isolated execution environment for one review — read-only checkout, run-scoped secrets only, egress allowlist, destroyed at run end. Its lifecycle is captured in audit entries, not persisted as a row.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A user can register a GitHub repo connector and trigger a review of an open PR, and a review is posted back to that PR, with no manual step between trigger and posted review.
- **SC-002**: 100% of review runs execute in a sandbox that, when probed, exposes **zero** host credentials/SSH keys/git config, refuses writes to the checkout, and refuses egress to any non-allowlisted host (verified by the isolation regression test).
- **SC-003**: The slice's repo connector exposes **no** push/commit/open-PR method (verified by a source-level regression pin).
- **SC-004**: Every successful review produces audit entries for provisioning, opencode invocation, and review posting; every failed review produces an audit entry for the failure.
- **SC-005**: A review of a typical PR completes (review posted or deterministic failure) within the configured wall-clock cap; exceeding the cap never leaves a residual sandbox or a partial review.
- **SC-006**: Malformed/empty opencode output never results in a posted review.
- **SC-007**: All new endpoints conform to the OpenAPI contract (`go test -tags contract ./cmd/...` passes) and the security-regression suite (`make sec-test`) passes for all slice pins.

## Assumptions

- The spec 003 agent runtime, effect+autonomy gate, and audit; the spec 004 vault and connector-registration/SSRF-validation patterns; and RLS tenant isolation are in place and reused unchanged.
- opencode can be invoked headlessly and configured to a constrained read/reason posture, and is baked into the sandbox container image (it need not be installed on the host).
- Docker is available wherever this slice runs (dev macOS and the target host); the Kubernetes-Job sandbox backend is a later slice.
- The LLM provider/key used by opencode is the agent's configured BYO provider credential (from the vault), injected run-scoped; the sandbox's only required egress is to that provider's API endpoint.
- GitHub authentication is a fine-grained PAT (`contents:read` + `pull_requests:write` + `metadata:read`); GitHub App installation tokens are deferred.
- The reviewed repository's contents are **untrusted**; isolation, not opencode's good behavior, is the security boundary.

## Out of Scope (this slice; later in spec 007 or beyond)

- Authoring code / opening pull requests / any repository write (the "Mode 1, PRs not silent pushes" path). This slice has no code-write capability at all.
- GitLab repo connector.
- GitHub webhook auto-trigger (PR opened/synchronized) — fast follow, reuses spec 004 webhook verification.
- Kubernetes-Job sandbox backend (interface is defined now; backend deferred).
- Inline, line-level review comments (this slice posts one review: summary + findings list).
- opencode wrapped as an MCP server / opencode consuming ManyForge MCP tools.
- Deduplication / supersession of in-flight or repeated reviews for the same PR.
