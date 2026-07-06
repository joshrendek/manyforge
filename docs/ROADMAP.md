# ManyForge Program Roadmap

**Status:** Approved (program-level design)
**Date:** 2026-05-31
**Scope:** Sequencing and decomposition for specs 002+ that build on the
tenant foundation (spec 001).

---

## Purpose

ManyForge is an open-source, all-in-one SMB/founder platform: a master business
with arbitrarily nested sub-businesses, AI support, inbound email, ticketing, a
lite CRM, and feature-request/bug-report flows — plus AI agents (both support/ops
agents and opencode coding/review agents) that operate on **native** ManyForge data
**and** against external systems (Jira, Zendesk, GitHub).

Spec 001 (`specs/001-tenant-foundation/`) is complete and establishes the spine:
identity, the business hierarchy, RBAC, the principal model (humans **and** agents),
dual-enforced tenant isolation, and an append-only audit trail. This roadmap
decomposes the remaining product into a dependency-ordered series of feature specs,
each cut as a **vertical slice** that ships end-to-end and is locked behind
regression tests before the next slice starts.

This is a program-level document. Each numbered spec below gets its own
`specs/NNN-name/` directory (spec.md → plan.md → tasks.md) via the Spec Kit flow
when we brainstorm it.

---

## Inherited constraints (non-negotiable)

Every spec MUST honor the foundation and the constitution
(`.specify/memory/constitution.md`):

- **Tenant isolation, twice.** Every tenant-owned row carries `tenant_root_id`
  (and a `business_id` where applicable); reads/writes are scoped both by SQL
  predicate **and** by self-deriving Postgres RLS. New tenant tables get RLS
  policies + the `authorized_businesses`/`authorized_tenants` helpers.
- **No existence oracle.** Unknown and unauthorized resources return the same
  404. Never 403 for tenant authorization.
- **Typed errors.** Service boundaries return `errs.ErrNotFound` /
  `ErrForbidden` / `ErrValidation` / `ErrConflict`; handlers branch with
  `errors.Is`. Raw `err.Error()` is never echoed except validation messages.
- **Audit in-transaction.** Every administrative and agent mutation writes an
  immutable `audit_entry` row in the same transaction as the change. Agent
  actions populate `inputs` / `outputs` / `decision`.
- **Test-first.** TDD (red → green → refactor). Merge gate is `make test` +
  `make int-test` + `make sec-test` + `make contract-test` + Playwright e2e, all
  green. No "pre-existing failure" exemption.
- **Modular monolith.** One binary under `cmd/manyforge`. Thin handlers →
  services → sqlc. Modules talk through interfaces, not each other's tables.
- **Bounded agents.** Agents are principals scoped to one business, run behind
  the autonomy gate (default Mode 1: sandboxed + human approval for
  external/irreversible actions), and every action — proposed and executed — is
  audited.
- **Open-core.** Community core under `internal/` is MIT and fully functional
  standalone. Enterprise features live under `ee/` behind extension points.
- **API conventions.** Base path `/api/v1`; EdDSA JWT + refresh rotation;
  cursor pagination (default 50, cap 100); stable error codes.

---

## Architecture decisions (this program)

Two forks were resolved up front because they shape the data model and every spec:

1. **Integration model — Native system-of-record + connectors (hybrid).**
   ManyForge owns canonical tickets / contacts / feedback. Connectors sync
   to/from Jira, Zendesk, GitHub, etc., and expose those systems as agent tools.
   The product works fully standalone (a self-hoster with no Jira), and *also*
   works against external systems.

2. **AI gateway — Provider-agnostic, BYO keys, local OK.**
   The gateway abstracts Anthropic / OpenAI / local (Ollama, vLLM). Credentials
   are per-tenant in the vault. Self-hosters can run fully local. Matches the
   open-core, self-hosters-first stance of the constitution.

3. **Build order — Vertical slices, support track first.**
   Each spec cuts through the layers and ends in a demoable, fully-tested
   thread. After the agent runtime (003), the **support track** (004 → 005 → 006)
   is built before the **coding track** (007).

---

## Shared layers (the cross-cutting spine)

These layers are **not** built up front — that would be premature abstraction and
would contradict the vertical-slice approach. Each is introduced *thin* in the
slice that first needs it, then *hardened* as later slices consume more of it.
The **layer-contract tests** (see Testing strategy) are the mechanism that keeps
earlier slices working when a shared layer grows.

| ID | Layer | Home package(s) | First introduced |
|----|-------|-----------------|------------------|
| **SL-A** | **Agent Runtime & AI Gateway** — provider abstraction, per-tenant BYO keys, model registry, token/cost accounting, run loop, **tool registry** (internal tools + connector tools + MCP host), **autonomy-gate implementation**, **approvals queue**, per-run audit | `internal/agents`, `internal/platform/ai` | Spec 003 |
| **SL-B** | **Connectors & Credential Vault** — envelope-encrypted per-tenant secrets, OAuth2 + API-key connections, typed connector interface (ticketing / repo / crm capabilities), signed inbound webhooks, sync engine (external-id mapping, conflict resolution) | `internal/connectors`, `internal/platform/secrets` | Spec 004 |
| **SL-C** | **Eventing & Activity Timeline** — in-process pub/sub + transactional **outbox** (reliable side-effects in the source write's transaction), unified "what happened on this entity" stream reused by tickets / contacts / feedback | `internal/platform/events` | Spec 002 (thin) |
| **SL-D** | **Notifications** — templated outbound (extends 001's `Mailer`), in-app notifications, per-user preferences, digesting | `internal/platform/notify` | Spec 002 (thin) |
| **SL-E** | **Attachments / Object Storage** — local FS (self-host) + S3-compatible, MIME-sniffed on the first 512 bytes, tenant-scoped | `internal/platform/blob` | Spec 002 (thin) |

All package names above are already anticipated by the constitution's module map.

---

## Spec sequence

```
001 ✅ Tenant Foundation
        │  identity · hierarchy · RBAC · principal(human+agent) · isolation · audit
        ▼
002  Native Support Desk            inbox + ticketing  (+ thin SL-C/D/E)
        ▼
003  Agent Runtime & AI Gateway     SL-A, first applied to 002's tickets
        ▼
004  External Ticketing Connectors  SL-B  (Jira / Zendesk + vault + sync)
        ▼
005  Lite CRM + Activity Timeline    crm  (SL-C hardened)
        ▼
006  Feedback / Feature-Request Boards   feedback
        ▼
007  Coding & Review Agents (opencode)   agents/coding  (reuses SL-A + SL-B)
        ⋯
(008 stretch) Founder Copilot — cross-business read-only assistant on SL-A
```

After 003 the runtime is a domain-agnostic capability layer, so the support track
and coding track both hang off it. We build support first; 007 reuses the vault +
connector framework SL-B that 004 introduces, adding a repo connector and a
sandboxed execution host.

### 002 — Native Support Desk
- **Modules:** `internal/inbox`, `internal/ticketing`; thin first cut of SL-C/D/E.
- **Scope:** per-business inbound email address; ingest (webhook/SMTP) → parse →
  thread; native ticket (status, queue, priority, assignment, message thread);
  attachments; outbound reply via notifications. **No AI yet** — a usable human
  support desk.
- **Depends on:** 001.
- **Demo:** email arrives → ticket created and threaded → human replies → email
  sent.
- **Regression contract:** ticket/inbox tenant isolation + RLS matrix; email-thread
  idempotency (same message-id never double-creates); audit on every ticket
  mutation; attachment MIME-sniff allowlist; outbox delivers reply exactly once.
- **Size:** L.

### 003 — Agent Runtime & AI Gateway
- **Modules:** `internal/agents`, `internal/platform/ai` (SL-A).
- **Scope:** provider abstraction (Anthropic/OpenAI/Ollama/vLLM) with per-tenant
  BYO keys; agent definitions (model, system prompt, allowed tools, autonomy
  mode) bound to one business; run loop with a tool registry (internal tools +
  MCP host); **autonomy-gate implementation** (the seam wired in 001) that
  classifies external/irreversible actions and routes them to an **approvals
  queue**; per-run audit via `inputs`/`outputs`/`decision`. First application:
  AI triage on 002's tickets (classify/priority/tag + draft reply, Mode 1).
- **Depends on:** 001; applies to 002.
- **Demo:** ticket arrives → agent proposes tags + a drafted reply → human
  approves in the queue → reply sent.
- **Regression contract:** deterministic agent runs via a **mock/recorded LLM
  provider** (golden fixtures); autonomy-gate **fail-closed** pin (gate runs after
  RBAC, before any tool execution); every agent action audited; agent run records
  tenant-isolated; agent cannot invoke a tool outside its allowed set.
- **Size:** L.

### 004 — External Ticketing Connectors (Jira + Zendesk)
- **Modules:** `internal/connectors`, `internal/platform/secrets` (SL-B).
- **Scope:** envelope-encrypted credential vault; OAuth2 + API-key connection
  setup; signed inbound webhooks; bidirectional sync between native tickets and
  Jira/Zendesk (external-id mapping, conflict resolution); connector tools so 003
  agents can act on external tickets.
- **Depends on:** 002 (ticket model), 003 (agent tools, optional).
- **Demo:** connect a Zendesk account → tickets sync both ways → an agent triages
  an external ticket through the autonomy gate.
- **Regression contract:** vault encryption + **no-secret-in-logs pin**; webhook
  signature verification; sync idempotency and conflict resolution; connector
  outbound calls go through the SSRF-safe client; calling an external system is an
  external/irreversible action → autonomy-gated + audited.
- **Size:** M–L.

### 005 — Lite CRM + Activity Timeline
- **Modules:** `internal/crm`; SL-C hardened.
- **Scope:** contacts, companies, deals/pipeline; a unified activity timeline
  auto-populated from inbox emails and tickets; AI enrichment / draft follow-ups
  via 003; optional HubSpot/Salesforce connectors reuse SL-B.
- **Depends on:** 002 (email/ticket activity), 003 (AI), 004 (connector pattern).
- **Demo:** an inbound email auto-creates/links a contact; the contact's timeline
  shows emails + tickets; an agent drafts a follow-up.
- **Regression contract:** contact dedup/merge correctness; cross-source timeline
  ordering/attribution; tenant isolation; enrichment behind the autonomy gate.
- **Size:** M.

### 006 — Feedback / Feature-Request Boards
- **Modules:** `internal/feedback`.
- **Scope:** per business/product board; submission (internal + optional public
  portal); voting; status workflow; dedupe; convert a request → ticket (links to
  002); AI clustering/dedupe via 003; optional GitHub-Issues sync via SL-B.
- **Depends on:** 002 (ticket convert), 003 (AI), 004 (connector pattern).
- **Demo:** a public submission is deduped against existing requests, voted on,
  and converted to a ticket.
- **Regression contract:** **public-portal oracle boundary** (an unauthenticated
  submission must not leak which businesses/products exist); voting integrity (one
  vote per identity); ticket-link integrity; tenant isolation of boards.
- **Size:** S–M.

### 007 — Coding & Review Agents (opencode)
- **Modules:** `internal/agents/coding`; reuses SL-A + SL-B.
- **Scope:** repo connector (GitHub/GitLab) via SL-B; **ephemeral sandboxed
  workspace with no ambient credentials**; opencode invoked through the MCP tool
  host from 003; output is **PRs only** (Mode 1, never a silent push) plus code
  reviews; runs gated and audited.
- **Depends on:** 003 (runtime + tools), 004 (vault + connector framework).
- **Demo:** point an agent at a repo + an issue → it works in a sandbox → opens a
  PR for human review.
- **Regression contract:** sandbox isolation pin (no credential/network leakage
  out of the sandbox); **no-ambient-creds pin**; **no-silent-push pin** (Mode-1
  output is a PR, never a direct push); opencode invocation contract; push/PR
  actions autonomy-gated; every coding action audited.
- **Size:** L.

### 008 — Founder Copilot (stretch)
A cross-business, read-only conversational assistant built on SL-A with read tools
spanning the verticals ("ask your portfolio anything"). May fold into 003. Listed
to mark the intent, not yet scoped.

---

## New `internal/` modules (cumulative)

| Module | Introduced | Purpose |
|--------|-----------|---------|
| `internal/inbox` | 002 | Inbound email ingest, parse, thread, route |
| `internal/ticketing` | 002 | Native tickets, queues, statuses, assignment, threads |
| `internal/platform/events` | 002 | Event bus + transactional outbox + activity timeline (SL-C) |
| `internal/platform/notify` | 002 | Templated + in-app notifications (SL-D) |
| `internal/platform/blob` | 002 | Attachment / object storage (SL-E) |
| `internal/agents` | 003 | Agent definitions, run loop, autonomy gate, approvals (SL-A) |
| `internal/platform/ai` | 003 | LLM provider abstraction + gateway (SL-A) |
| `internal/connectors` | 004 | Connector interface, sync engine, webhooks (SL-B) |
| `internal/platform/secrets` | 004 | Encrypted per-tenant credential vault (SL-B) |
| `internal/crm` | 005 | Contacts, companies, deals, activity timeline |
| `internal/feedback` | 006 | Feature-request / bug-report boards |
| `internal/agents/coding` | 007 | opencode coding/review agents + sandbox host |

---

## Testing & regression strategy

The explicit priority: **catch regressions as shared layers change between slices.**
Mechanisms:

- **Layer-contract suites.** Each shared layer (SL-A tool registry, SL-B connector
  interface, SL-C event bus, SL-D notify, SL-E blob) gets an interface-contract
  test that *every* implementation and *every* consumer must satisfy. Changing a
  layer runs its contract suite plus all consumers' integration tests. Lives behind
  a new `make contract-test` target.
- **Source-level pins** (extending today's `internal/security_regression/*_pin_test.go`):
  one file per guarantee, with the guarantee in the header — autonomy-gate-runs-
  before-execution, no-secret-logging, no-ambient-creds, no-silent-push,
  no-existence-oracle. A refactor that drops a guard fails CI loudly.
- **Deterministic AI.** A mock/recorded LLM provider produces golden agent-run
  fixtures, so prompt/model changes surface as fixture diffs rather than flaky
  behavior.
- **Recorded HTTP cassettes** (go-vcr style) for Jira/Zendesk/GitHub so external
  API contract drift is caught without live network calls.
- **Per-spec CI gate.** `make test` + `make int-test` + `make sec-test` +
  `make contract-test` + Playwright e2e, all blocking. No exemptions.

Each spec's "regression contract" (listed per card above) enumerates the specific
pins and contract tests that spec must add before it is considered done.

---

## Out of scope / parallel tracks

- **Enterprise (`ee/`):** SSO/SAML/SCIM, premium connectors, advanced compliance.
  Behind extension points; the community build must remain fully functional
  without `ee/`.
- **Per-node role overrides** and **delegated per-node role authoring** —
  enhancements noted but deferred by spec 001.

---

## Next step

Brainstorm **Spec 002 — Native Support Desk** through the normal
brainstorm → spec → plan → tasks flow, producing `specs/002-support-desk/`.
Optionally, file `bd` epics for 002–007 to track the program in the issue tracker.

<!-- retrigger: verify per-dimension provider routing live on hub (manyforge-azy) -->
