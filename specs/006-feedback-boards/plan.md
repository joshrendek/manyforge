# Spec 006 — Feedback / Feature-Request Boards (Slice 1)

Epic: `manyforge-saz` · Slice issue: `manyforge-saz.1` · Module: `internal/feedback`

Builds on the support-desk (Spec 002, ticket convert), agent runtime (Spec 003, deferred
AI clustering), and connector pattern (Spec 004, the public-ingress template). Follows the
Spec 005 CRM module as the structural template.

## Goal

A business runs one or more **feedback boards**. Users submit **posts** (feature requests /
ideas / bug reports), the business moves them through a **status workflow**, users **vote**
(one vote per identity), and a post can be **converted to a support ticket** (Spec 002).
Apple/Android SDKs (and a future public web portal) submit + vote via a **public ingress
API** authenticated by a per-board **publishable key**.

## Scope (this slice)

IN:
- Boards CRUD (authenticated, business-scoped).
- Posts: submit / list / get / moderate status / soft-delete (authenticated).
- Voting: one vote per identity per post; denormalized `vote_count` maintained in the
  same tx as the vote.
- Convert post → ticket (links Spec 002); idempotent.
- Publishable ingest keys (create / list / revoke, authenticated).
- **Public SDK ingress API** (principal-less): submit post, list public posts, vote —
  keyed by a publishable board key.
- OpenAPI contract + drift test; security-regression pins; integration + unit tests.

OUT (tracked follow-ups saz.2–saz.5):
- AI clustering/dedupe (Spec 003), GitHub-Issues sync (SL-B), Angular UI, signed
  server-to-server ingest, status-change notifications.

## Data model (migration `0102`)

Business-scoped (like tickets) → RLS predicate
`business_id IN (SELECT business_id FROM authorized_businesses(current_principal()))`.
Every table carries `tenant_root_id`, a `UNIQUE (id, tenant_root_id)`, tenant-consistent
composite FKs, and a `*_troot_immutable` trigger.

- `feedback_status` enum: `open | planned | in_progress | done | declined`.
- `feedback_board(id, business_id, tenant_root_id, slug, name, description, is_public,
  created_at, updated_at)` — `UNIQUE (business_id, slug)`.
- `feedback_post(id, business_id, tenant_root_id, board_id, title, body, status,
  vote_count, author_kind {principal|public}, author_principal_id, author_identity,
  ticket_id, created_at, updated_at, deleted_at)` — FK `(board_id, tenant_root_id)`,
  FK `(ticket_id, tenant_root_id) → ticket` (**ticket-link integrity**).
- `feedback_vote(id, business_id, tenant_root_id, post_id, voter_identity, created_at)` —
  `UNIQUE (post_id, voter_identity)` (**voting integrity: one vote per identity**),
  FK `(post_id, tenant_root_id)`.
- `feedback_ingest_key(id, business_id, tenant_root_id, board_id, publishable_key, label,
  status {enabled|revoked}, created_at, revoked_at)` — `UNIQUE (publishable_key)`.
  The key is **publishable** (safe to embed in an app binary); it is not a secret.

### SECURITY DEFINER functions (principal-less public ingress; `SET search_path = public`)

- `feedback_public_board(p_key text)` → board tenancy + `is_public`; only enabled key on a
  public board returns a row (else 0 rows → uniform 401).
- `feedback_public_submit(p_key, p_title, p_body, p_author_identity)` → new post id.
- `feedback_public_vote(p_key, p_post_id, p_voter_identity)` → bool accepted
  (`INSERT … ON CONFLICT (post_id, voter_identity) DO NOTHING`; bumps `vote_count` only when
  newly inserted — voting integrity holds even under the RLS-bypassing DEFINER path).
- `feedback_public_list_posts(p_key, p_limit)` → top posts by `(vote_count desc, created_at desc)`.
- `convert_feedback_post_to_ticket(p_post_id, p_business_id, p_tenant_root)` → ticket id
  (idempotent; creates requester + ticket, links `feedback_post.ticket_id`). Invoked from
  the authenticated service **after** an RLS-bound fetch confirms the caller can see the post.

All DEFINER fns: `REVOKE ALL … FROM PUBLIC; GRANT EXECUTE … TO manyforge_app`.

## API surface

Authenticated (`/api/v1`, `RequireAuth` + permission gate), business-scoped under `{id}`:
- read (`feedback.read`): list boards, get board, list posts, get post, list ingest keys.
- write (`feedback.write`): create/update board, create ingest key, revoke ingest key,
  set post status, soft-delete post, convert post → ticket.

Permissions (`internal/authz/perms.go` + migration): `feedback.read` (owner/admin/member/
viewer), `feedback.write` (owner/admin), mirroring `crm.read`/`crm.write` (migration 0058).

Public ingress (`/api/v1/feedback/public/{key}`, in the `ingress` group behind
`ingestLimit`, principal-less):
- `POST /posts` — submit `{title, body?, author_identity?}` → 201 `{id, title, status, vote_count}`.
- `GET  /posts` — list public posts → `{items:[{id, title, body?, status, vote_count, created_at}]}`.
- `POST /posts/{postID}/votes` — `{voter_identity}` → 200 `{voted, vote_count}`.

### Oracle policy (public-portal boundary)

- Unknown / revoked key, or a key on a non-public board → **uniform 401** (never reveals
  business/board existence; publishable keys are opaque-random so slugs can't be enumerated).
- Body over cap → 413. Malformed body on a valid key → 400. Valid key → serve.
- Error bodies never echo board/business names.

## Test plan

- **Integration** (`//go:build integration`, `internal/platform/db/testdb`):
  - `feedback_board_integration_test.go` — board CRUD, tenant isolation (a second tenant's
    board is invisible → ErrNotFound).
  - `feedback_post_integration_test.go` — post lifecycle, status moderation, soft-delete,
    convert→ticket links `ticket_id` and is idempotent.
  - `feedback_vote_integration_test.go` — one-vote-per-identity (second identical vote is a
    no-op; `vote_count` matches distinct identities).
  - `feedback_public_integration_test.go` — public submit/list/vote via DEFINER; unknown key
    → no row (401 at handler); non-public board → 401; oracle uniformity.
- **Unit** (no build tag): handler input validation (400s), cursor round-trip, oracle
  handler responses (401 shape identical for unknown vs non-public).
- **Security regression** (`internal/security_regression/`, source-level grep pins,
  one file `feedback_*_pin_test.go`):
  - RLS: migration 0102 has `ENABLE ROW LEVEL SECURITY` + `authorized_businesses(current_principal())`
    for each table; denylist `authorized_tenants` (feedback is business-scoped).
  - Tenant predicate: every id-taking query in `db/query/feedback.sql` contains `tenant_root_id =`.
  - Voting integrity: `UNIQUE (post_id, voter_identity)` present in 0102.
  - Oracle: public handler returns 401 for unknown/non-public key; DEFINER `feedback_public_board`
    filters `status='enabled'` AND `is_public`.
  - DEFINER hardening: every `SECURITY DEFINER` fn keeps `SET search_path`.
  - Perms: `feedback.read`/`feedback.write` constants appear in a migration (extends the
    existing perms-in-migration pin).
- **Contract**: `specs/006-feedback-boards/contracts/openapi.yaml` + `cmd/manyforge/drift_006_test.go`
  (two-way, `//go:build contract`) + a skip guard in `TestOpenAPIDrift`.

## Gates before "done"

`make generate && go build ./... && make test && make contract-test && make sec-test` all
green; feedback integration tests green; app boots locally and a curl demo exercises
create-board → create-key → public-submit → vote → list → convert-to-ticket.
