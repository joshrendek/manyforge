<!--
SYNC IMPACT REPORT
==================
Version change: 1.0.0 → 1.0.1
Bump rationale (1.0.1): PATCH — resolved the deferred TODO(FRONTEND_FRAMEWORK) by recording
  the frontend framework as Angular, exactly as 1.0.0 anticipated ("recorded here by
  amendment"). No principle, section, or governance change.
Bump rationale (1.0.0): First concrete ratification of the constitution (template
  placeholders replaced with project-specific governance). Initial adoption → 1.0.0 (MAJOR).

Principles (template slot → ratified name):
  PRINCIPLE_1 → I. Tenant Isolation & Hierarchy Integrity (NON-NEGOTIABLE)
  PRINCIPLE_2 → II. Security & Data Privacy by Default (NON-NEGOTIABLE)
  PRINCIPLE_3 → III. Test-First, Automated Verification (NON-NEGOTIABLE)
  PRINCIPLE_4 → IV. Bounded, Auditable AI Agents
  PRINCIPLE_5 → V. Modular Monolith & Service-Layer Architecture
  (added)     → VI. Observability & Auditability
  (added)     → VII. Open Source, Open-Core, & Community Trust

Sections:
  SECTION_2 → Technology & Architecture Constraints
  SECTION_3 → Development Workflow & Quality Gates
  (kept)    → Governance

Added sections: none beyond the two named slots (principle count expanded 5 → 7).
Removed sections: none.

Templates / artifacts reviewed for consistency:
  ✅ .specify/templates/plan-template.md — Constitution Check gate references the
       constitution dynamically; no edit needed. Principle-derived gates listed below.
  ✅ .specify/templates/spec-template.md — aligned; tenancy/security/agent requirements
       are expressible as functional requirements; no edit needed.
  ✅ .specify/templates/tasks-template.md — UPDATED: test tasks changed from OPTIONAL to
       MANDATORY to match Principle III (Test-First, NON-NEGOTIABLE).
  ✅ .specify/templates/checklist-template.md — generic; no constitution-specific edit.
  ➖ No README.md / docs/quickstart.md / commands templates present to update.

Deferred / follow-up TODOs:
  ✅ RESOLVED (1.0.1): FRONTEND_FRAMEWORK → Angular (latest stable), TypeScript SPA under
    `web/` consuming the Go JSON API. See Technology & Architecture Constraints → Frontend.
-->

# ManyForge Constitution

ManyForge is an open-source, all-in-one platform for SMBs and founders: a master business
that contains many sub-businesses, each with AI agents, inbound email, ticketing, a lite
CRM, a feature-request / bug-report flow, and opencode-powered coding/review agents. The
principles below are non-negotiable engineering and product law for the project.

## Core Principles

### I. Tenant Isolation & Hierarchy Integrity (NON-NEGOTIABLE)

ManyForge is multi-tenant with a master-business → sub-business hierarchy, and isolation
is structural, not best-effort.

- Every tenant-owned row MUST carry a `business_id`. Every read and write MUST be scoped to
  the caller's authorized business subtree, with the predicate pushed into SQL
  (`WHERE business_id = ANY($authorized_subtree)`), never relying on a handler-side check
  the query does not also enforce.
- PostgreSQL Row-Level Security (RLS) MUST be enabled on every tenant-owned table as
  defense-in-depth behind the application predicate. System-catalog rows
  (`business_id IS NULL`) MUST be marked with a `// security: system catalog` comment.
- Unknown IDs and IDs belonging to another tenant MUST return the identical not-found shape
  (404). The platform MUST NOT expose a 403-vs-404 existence oracle across tenants.
- Hierarchy mutations (moving a sub-business, granting cross-business access) MUST be
  explicit, transactional, and audited; access never leaks implicitly upward or sideways.

**Rationale**: A multi-tenant platform's single worst failure is cross-tenant data leakage.
Isolation enforced in SQL + RLS makes leakage impossible by construction rather than by
discipline.

### II. Security & Data Privacy by Default (NON-NEGOTIABLE)

The platform aggregates a founder's most sensitive data — customer email, CRM records,
tickets, agent transcripts — across many sub-businesses. A breach is existential.

- Service methods MUST take the caller's identity + business scope and enforce ownership in
  SQL. ALL caller-supplied UUIDs (body fields, not just URL params) MUST be ownership-checked
  before persistence.
- Errors MUST use typed sentinels (`ErrNotFound`/`ErrForbidden`/`ErrValidation`/`ErrConflict`)
  mapped to stable codes. Raw `err.Error()` MUST NOT be echoed to clients except for typed
  validation messages; wrapped errors are logged server-side only.
- Auth tokens: JWT validation MUST pin algorithm, issuer, and audience. Refresh tokens MUST
  rotate with reuse detection. Secret/signature comparisons MUST be constant-time.
- Input MUST be validated against strict allowlists; uploads MIME-sniffed; URLs built via
  encoders, never `fmt.Sprintf` of user input. Any outbound HTTP influenced by user or agent
  input MUST use an SSRF-guarded client that refuses RFC1918, loopback, link-local, and
  cloud-metadata addresses.
- Secrets MUST NOT be committed. Credential-bearing query params MUST be redacted before any
  request URI is logged; request bodies and tokens MUST NOT be logged.

**Rationale**: These are the load-bearing controls for a system holding regulated PII for
many businesses at once; they are cheap to keep and catastrophic to omit.

### III. Test-First, Automated Verification (NON-NEGOTIABLE)

Correctness is proven by automated tests, never by manual click-through.

- TDD is mandatory: write the test, watch it fail, then implement (Red-Green-Refactor). No
  feature or fix merges without automated coverage of its behavior.
- Manual verification is never the verification of record. UI changes MUST be exercised in a
  real browser AND codified as a regression test (e.g. a Playwright spec).
- Security fixes MUST follow the three-commit cadence: (1) characterization tests pinning
  surrounding behavior, (2) an exploit test demonstrating the bug, (3) the fix, after which
  the exploit test inverts into a regression assertion. Regression tests live in a dedicated
  package with the finding ID in a header comment.
- The merge gate is `make test` + `make lint` + `make sec-test`, all green. There is no
  "pre-existing failure" exemption — a red test blocks the merge regardless of who broke it.

**Rationale**: An AI-agent-heavy platform changes fast and non-deterministically; an
automated test suite is the only durable guardrail against silent regression.

### IV. Bounded, Auditable AI Agents

Agents are the platform's highest-leverage and highest-risk surface. Autonomy is earned and
bounded, never ambient.

- Every agent is scoped to exactly one business in the hierarchy and inherits that business's
  tenant isolation (Principle I). An agent MUST NOT be able to reach another tenant's data.
- Agent autonomy is a configurable policy with three modes: **(1)** autonomous inside
  sandboxed, read-mostly scopes with human approval required for external or irreversible
  actions; **(2)** fully supervised (every action approved); **(3)** broadly autonomous within
  tenant scope. **New agents default to Mode 1.** The autonomy level MUST be enforced
  server-side and MUST NOT be client-trusted.
- External or irreversible actions — sending email, publishing publicly, merging/pushing
  code, deleting data, mutating billing — MUST be gated behind explicit human approval unless
  the agent's policy explicitly permits them.
- opencode coding/review agents MUST run in isolated, ephemeral workspaces with no ambient
  credentials. Under Mode 1 their repo changes surface as proposed diffs / pull requests,
  never as silent pushes.
- LLM output is untrusted input: it MUST NOT be interpolated into SQL, shell, or URLs, and
  every agent-initiated tool call MUST pass through the same authorization layer as a human
  request.
- Every agent action — proposed and executed — MUST be written to an immutable audit log
  (actor=agent, business, action, inputs, outputs, decision, timestamp).

**Rationale**: Configurable-but-bounded autonomy plus a complete audit trail is what makes it
safe to let agents act on customer data and source code.

### V. Modular Monolith & Service-Layer Architecture

ManyForge ships as a single Go deployable that an SMB can self-host trivially, while keeping
clean internal seams.

- One deployable binary under `cmd/`. Code lives under `internal/`, with each capability —
  tenancy, crm, ticketing, inbox (inbound email), agents, feedback (feature-request/bug),
  platform (auth/db/errs) — a bounded module exposing a clear interface.
- HTTP handlers MUST stay thin (validate → call service → return JSON). Business logic lives
  in services. Database access MUST be type-safe via sqlc; generated code MUST NOT be
  hand-edited (`make generate` after SQL changes). PostgreSQL is the system of record.
- Modules MUST communicate through interfaces, not by reaching into one another's tables.
  Writes that must be atomic across the source-of-truth and derived state MUST share a
  single transaction.
- Enterprise features live under `ee/` behind extension points; the community build omits
  `ee/` and MUST remain fully functional on its own.

**Rationale**: A modular monolith gives self-hosters a single binary while preserving the
seams needed for the open-core split and any future service extraction.

### VI. Observability & Auditability

Operators and tenants MUST be able to answer "who or what did this, and when" for any record.

- Structured logging (slog or zerolog) with business / request / agent correlation IDs;
  errors wrapped with context.
- Every admin and agent mutation MUST write an audit-log row (actor, target, action,
  old→new value, timestamp) inside the same transaction as the change it records.
- Denormalized state (counters, last-X timestamps) MUST be updated in the same transaction as
  the source-of-truth write — never fire-and-forget; a failed derived write rolls the whole
  transaction back.
- Health, readiness, and metrics endpoints MUST exist. The inbound-email and agent pipelines
  MUST be traceable end-to-end.

**Rationale**: When agents and many sub-businesses act on shared infrastructure, an immutable,
transaction-coupled audit trail is the difference between an answerable incident and a guess.

### VII. Open Source, Open-Core, & Community Trust

Trust is the product for a platform holding a founder's whole business; open development and
an honest open-core line earn it.

- The core (`internal/` and the default build) is licensed **MIT** and MUST be fully
  functional standalone. Enterprise features under `ee/` carry a separate commercial /
  source-available license. The open/EE boundary MUST be explicit in directory layout and
  build tags; the community edition never depends on `ee/` to run.
- Development happens in the open on GitHub: public issues, a transparent roadmap, documented
  architecture, semantic-versioned releases, and a maintained CHANGELOG.
- Contributions require a Developer Certificate of Origin (DCO) sign-off. Contributions to
  `internal/` are MIT-licensed; contributions to `ee/` are under the EE license.
- Self-hosters are first-class. Telemetry MUST be opt-in. Secrets and keys MUST NOT ship in
  the repository.

**Rationale**: An honest, enforceable boundary between a genuinely-open core and a commercial
tier sustains both community trust and the project's longevity.

## Technology & Architecture Constraints

- **Language / runtime**: Go (latest stable). Single deployable binary built from `cmd/`.
- **Persistence**: PostgreSQL as the system of record. Type-safe queries via sqlc; schema
  migrations are versioned, forward-only, and run in CI. RLS enabled on tenant tables.
- **Layout**: `cmd/` (entrypoints), `internal/` (open-core modules), `ee/` (commercial
  features behind extension points), `web/` (dashboard UI). `internal/` packages do not
  import `ee/`.
- **Tenancy**: shared-schema multi-tenancy; `business_id` on every tenant-owned table; the
  business hierarchy stored as a closure/path structure for efficient subtree scoping.
- **AI / agents**: pluggable LLM provider behind an interface. opencode is invoked across a
  process/API boundary inside ephemeral, credential-less sandboxes. Agent autonomy enforced
  per Principle IV.
- **Inbound email**: handled by the `inbox` module; bodies and attachments size-capped and
  MIME-sniffed; parsing failures degrade safely.
- **Outbound HTTP**: a single SSRF-guarded, HTTPS-only client for all user/agent-influenced
  fetches (email link expansion, webhooks, agent tool calls).
- **Frontend**: the web dashboard is an **Angular** (latest stable) single-page application
  written in TypeScript, living under `web/` and consuming the Go JSON API. The frontend is
  never a security boundary — all authorization and tenant scoping are enforced server-side
  (Principles I & II); the SPA holds no secrets and trusts no client-side access check.

## Development Workflow & Quality Gates

- **Spec-driven**: every non-trivial feature flows through Spec Kit — specify → (clarify) →
  plan → tasks → implement. The plan's **Constitution Check** gate MUST pass before research
  and again after design. Principle-derived gates to verify in every plan:
  - Tenant isolation: data model carries `business_id` and queries scope by subtree (I).
  - Security: ownership predicates, typed errors, validated inputs, SSRF guards (II).
  - Tests: a concrete test plan (unit/integration/contract/e2e) exists; tests precede
    implementation (III).
  - Agents: any agent capability declares its autonomy mode, sandbox, and audit hooks (IV).
  - Architecture: changes respect module boundaries and the thin-handler/service split (V).
  - Observability: mutations write audit rows in-transaction; correlation IDs present (VI).
  - Open-core: enterprise-only code is confined to `ee/`; community build stays whole (VII).
- **Branching & review**: feature branches; pull requests reviewed before merge. CI runs
  `make test` + `make lint` + `make sec-test`; all green is required to merge.
- **Security fixes**: follow the three-commit cadence (Principle III); regression tests in a
  dedicated package with the finding ID.
- **Pre-commit**: run tests, linters, and available security scanners before committing.
- **Post-ship**: update documentation (README/ARCHITECTURE/CHANGELOG) to match what shipped.

## Governance

- This constitution supersedes other practices. Where any document, convention, or habit
  conflicts with it, the constitution wins until amended.
- **Amendments** are proposed via pull request and require maintainer approval plus a written
  impact note (what changes, why, and any migration required for in-flight work). On merge,
  the version is bumped and dependent templates are re-synced in the same change.
- **Versioning** follows semantic versioning for governance:
  - **MAJOR**: backward-incompatible removal or redefinition of a principle or governance rule.
  - **MINOR**: a new principle/section is added or existing guidance is materially expanded.
  - **PATCH**: clarifications, wording, and non-semantic refinements.
- **Compliance**: every feature plan's Constitution Check gate verifies adherence. Any
  deviation MUST be justified in the plan's Complexity Tracking table with the simpler
  alternative and why it was rejected; unjustifiable deviations MUST be removed, not merged.
- **Runtime guidance**: agent-specific guidance files (e.g. `CLAUDE.md`) provide day-to-day
  development guidance and MUST stay consistent with this constitution; on conflict, this
  document governs.

**Version**: 1.0.1 | **Ratified**: 2026-05-29 | **Last Amended**: 2026-05-29
