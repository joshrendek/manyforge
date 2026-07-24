# Feedback Verified-Identity Tier + Read Path — Design

**Date:** 2026-07-24
**Spec:** 006 (Feedback / Feature-Request Boards) — follow-up
**bd:** `manyforge-saz.5` (verified-identity subset; SL-D status-change notifications tracked separately, out of scope here)
**Branch (to be cut fresh off `master`):** `006-feedback-verified-identity`
**Status:** approved design + Fable review (SHIP-WITH-FIXES, folded in) → awaiting spec review → `writing-plans`

## 1. Problem & scope

Spec 006 shipped anonymous public ingress: an app embeds a **publishable** key (`fbk_…`, a
client-side token like a Sentry DSN) and its end-users submit/vote via the public endpoints,
identified only by an opaque, caller-supplied `author_identity` / `voter_identity` string. That
string is untrusted — any client can send any value — so votes and submissions carry no real
identity, and a device that clears storage (or a bad actor) can squat or re-vote.

This follow-up adds a **verified-identity tier** for callers who route feedback through *their own
backend*, plus a **read path** so a client can render server-truth "did I vote" state and a
"my submissions" filter.

**In scope**
- Optional HMAC request signature that a customer's **backend** attaches, upgrading a request to
  `identity_verified=true`. Same public endpoints — signature is optional.
- A per-key **secret** (`fbs_…`), sealed at rest, minted at key creation, returned in plaintext once.
- Three-namespace identity model (internal-principal / `a:` anon / `v:` verified) so anonymous
  callers cannot squat verified (or internal) identities.
- Exactly-once submit via a signed `idempotency_key` + a consumed-set.
- Read path: `viewer_voted` flag per post + a `?author=` my-submissions filter on the public list.
- Frontend: admin secret-once UX + `has_secret` indicator + a "Verified" badge; portal passes
  `voter_identity` to get server-truth voted state.

**Out of scope (deferred, noted)**
- SL-D status-change notifications to subscribers/authors (`saz.5`'s other half) — separate round.
- convert→ticket stays **synthetic** (no CRM / requester / ticketing changes this round).
- Secret **rotation** endpoint. A verified-capable key is obtained by creating a *new* key while the
  master key is configured; pre-existing keys stay anon-only. **Adoption path (document it so nobody
  "fixes" it with retroactive minting):** an existing customer runs **two keys on one board** — the
  old `fbk_` key stays anon in the already-shipped app binary; a new `fbk_`+`fbs_` pair goes in their
  backend for the verified path.
- Idempotency-record **eviction sweeper** — the consumed-set carries `created_at`; a periodic
  `DELETE … WHERE created_at < now() - interval '7 days'` can be added later. Not required for
  correctness (the `PRIMARY KEY` guarantees exactly-once regardless of retention). When added,
  retention MUST exceed any client retry horizon; signed replays are independently bounded by 300s.
- Signing the GET **query string** on verified reads — see §6 for the documented residual.
- Author-filter for the *anonymous* namespace carries no confidentiality (public posts are already
  world-readable) — documented trade-off, not a security control.

## 2. Trust model

Two tiers over **one** set of endpoints:

| Tier | Who sends | Auth material | Result |
|------|-----------|---------------|--------|
| Anonymous (existing) | app client / portal browser | publishable key only | opaque identity, `identity_verified=false` |
| Verified (new) | customer's **backend** (server-to-server) | publishable key **+** `X-Feedback-Signature` | `identity_verified=true` |

The load-bearing distinction: the **publishable key ships inside the app binary** (client-side), so
it can never gate trust. The **secret must never ship to a client** — therefore the verified tier is
*inherently server-to-server*. A browser/mobile app stays on the anonymous path; only a customer
backend that holds the secret can sign. This mirrors Stripe/GitHub webhooks (publishable vs signing
secret).

## 3. Signature scheme

Stripe-webhook style, **extended to bind the request method + path** (a request authenticator, not
a fixed-URL webhook — binding method+path is nearly free now and impossible to retrofit later):

- **Header:** `X-Feedback-Signature: t=<unix-seconds>,v1=<hex>`
- **Signed payload:** the string `"<t>.<METHOD>.<path>.<raw-request-body>"` where `<path>` is the
  **full routed request path** the server sees (`r.URL.Path`) — this includes the **`/api/v1`
  prefix** and the `{key}` segment, and **excludes** the query string — and `<raw-request-body>` is
  the exact bytes read off the wire (before JSON decode; empty for GET). **Concrete example:** a
  submit signs over `<t>.POST./api/v1/feedback/public/fbk_ABC.../posts.{"title":"…","idempotency_key":"…"}`
  — NOT `/feedback/public/…` (a customer who omits `/api/v1` computes a wrong MAC and gets 401).
- **MAC:** `v1 = hex(HMAC_SHA256(secret, "<t>.<METHOD>.<path>.<body>"))`.
- **Compare:** `subtle.ConstantTimeCompare` against the recomputed MAC.
- **Replay window:** reject when `abs(now − t) > 300s`.
- **Outcomes (submit/vote):**
  - No `X-Feedback-Signature` header → **anonymous** (`identity_verified=false`). Never an error.
  - Header present, key valid, `sealed_secret` present, signature valid, within window → **verified**.
  - Header present but signature invalid / malformed / outside window, **on a key that has a
    secret** → **401** (deliberate: the secret was set and the payload is forged; mirrors
    `webhook.go`'s known-connector-bad-sig → 401).
  - Unknown / revoked key, or key on a non-public board → existing **uniform 401** (no oracle),
    regardless of signature presence — the board lookup fails first.

**Fail-closed matrix for the secret (do NOT collapse these three into one "degrade to anon"):**

| Condition | Signature presented? | Result |
|-----------|----------------------|--------|
| `sealed_secret IS NULL` (key predates feature / never minted) | yes or no | **anon** (nothing to verify); if a sig *was* sent, WARN-log once |
| `sealed_secret` present, but process `Sealer` is **nil** (master key unset in this env) | yes | **401** + ERROR log |
| `sealed_secret` present, `Sealer.Open` **fails** (wrong master key / tampered blob) | yes | **401** + ERROR log (mirrors `webhook.go:123-130`) |
| `sealed_secret` present, `Sealer` present, unseal OK | no | **anon** |

Rationale: silently reclassifying a *signing* caller's traffic as anon (because ops dropped the
master key, or the blob won't open) forks that user's identity history across the `a:`/`v:`
namespaces **permanently** and undetectably (votes double, `viewer_voted` wrong), and the caller
gets a 201 it cannot distinguish from success. A signing caller can handle a 401; it cannot undo a
silent downgrade. Only the genuine "no secret exists" case degrades.

**Replay & exactly-once.**
- **Time window** bounds every signed request (±300s).
- **Votes** are already idempotent (`ON CONFLICT (post_id, voter_identity) DO NOTHING`).
- **Submit** gains an optional **`idempotency_key`** in the request **body** — deliberately the body,
  not a header, so it is **covered by the signature**. An unsigned header would let an active
  attacker strip/alter it on a captured signed request and force a duplicate; a signed body field
  cannot be tampered without the secret. First submit with the (tier-namespaced) key creates the
  post and records the pair; a later submit with the same key returns the **same post**. See §7.
- Semantics: fresh → **201** with the new post; replay (same key, same body) → **200** with the
  existing post (`deduped: true`); replay with a **different body** → **409** (same key, different
  request — Stripe semantics; surfaces client misuse and blocks cross-request swallowing).

**Documented oracles (deliberate, acceptable):**
- `has_secret` probe: valid key + garbage signature → 401 iff a secret exists, else 2xx-as-anon.
  Knowing a secret *exists* doesn't help forge it.
- 200-vs-201 on submit reveals whether an idempotency key is in use — but only *within the caller's
  own tier namespace* (see §7), so it is not a cross-tier probe.

## 4. Secret storage & nil-guard

- New column `feedback_ingest_key.sealed_secret text NULL`.
- Secret plaintext format `fbs_…` (distinct prefix from the publishable `fbk_…`, so the two are never
  confused in customer config). Minted with crypto/rand, same shape as `newPublishableKey`.
- Sealed with `internal/platform/crypto.Sealer` (AES-256-GCM; `Seal`/`Open`), the exact primitive
  used for connector/DKIM/MCP/GitHub-App credentials.
- New master key env **`MANYFORGE_FEEDBACK_MASTER_KEY`**, wired through `internal/platform/config`
  via the existing `envKey32` helper as `Config.FeedbackMasterKey []byte`. Semantics mirror
  `ConnectorMasterKey` verbatim:
  - **Unset ⇒ `nil`** → verified tier **disabled**, server still boots, anonymous path fully
    unaffected. `CreateIngestKey` mints no secret (`sealed_secret` stays NULL).
  - **Set but not 32 bytes ⇒ hard config error** at startup (`envKey32`/`Load`).
  - Set & valid ⇒ a `*crypto.Sealer` is constructed and injected into the feedback service + public
    handler.
- **Minting:** `CreateIngestKey` mints+seals the secret **only when a `*crypto.Sealer` is present**,
  stores `sealed_secret`, and returns the **plaintext once** in the create response alongside the
  publishable key. Never retrievable again; never logged (regression-pinned).
- **Listing:** `ListIngestKeys` / `toIngestKey` expose **`has_secret bool`** (= `sealed_secret IS NOT
  NULL`) only — never the sealed blob, never the plaintext.
- No code path dereferences a nil `*crypto.Sealer`: nil ⇒ minting skipped, and per §3 a signed
  request against a secret'd key when the sealer is nil is a 401, not a deref.

## 5. Identity namespacing: three provably-disjoint namespaces

`feedback_vote.voter_identity` and `feedback_post.author_identity` are shared columns holding
**both** internal principal UUIDs (the authenticated `Vote`/submit paths in `post.go` write
`principalID.String()` raw) **and** public opaque identities. Against `UNIQUE (post_id,
voter_identity)` we define three namespaces:

| Namespace | Shape | Written by |
|-----------|-------|-----------|
| Internal | a bare UUID string, e.g. `7f3e…` (no `:`) | authenticated principal paths (unchanged) |
| Anonymous public | `a:<caller-identity>` | public DEFINER with `p_verified=false` |
| Verified public | `v:<caller-identity>` | public DEFINER with `p_verified=true` |

A UUID string never contains `:`, and the two public prefixes are `a:`/`v:`, so the three sets are
**provably disjoint**. **The prefix is applied inside the SECURITY DEFINER function from the
authoritative `p_verified` flag** — never by the Go caller, never derived from the caller's raw
string. An anonymous caller passing raw `v:alice` is stored `a:v:alice` — still anon, cannot collide
with verified `v:alice`. This also closes a *pre-existing* hole where an anon public voter could
squat an internal principal's vote by supplying that principal's UUID.

**Backfill (required — otherwise legacy rows double-vote and reads break).** Migration 0104 must
prefix existing *public* rows into `a:` while leaving internal principal-UUID rows raw:

- `feedback_vote`: `UPDATE … SET voter_identity = 'a:' || voter_identity WHERE voter_identity NOT IN
  (SELECT id::text FROM principal)`. Rationale: votes carry no author-kind column, so membership in
  `principal` is the only discriminator. The residual — a public anon voter who *coincidentally or
  maliciously* supplied a real principal's UUID as their identity — was **already** a squat/collision
  against that principal pre-0104 (same `UNIQUE`), so leaving those rows raw neither creates nor
  worsens exposure; document it.
- `feedback_post`: `UPDATE … SET author_identity = 'a:' || author_identity WHERE author_kind =
  'public' AND author_identity IS NOT NULL`. The `feedback_post_author_chk` CHECK guarantees
  principal posts have `author_identity IS NULL`, so `author_kind='public'` is an exact, safe
  discriminator here (no principal-UUID ambiguity).
- **Down migration** strips the `a:`/`v:` prefixes symmetrically (documented as best-effort: a
  legacy raw identity that genuinely began with `a:`/`v:` — vanishingly unlikely — would be
  over-stripped; acceptable for a down path).

Going forward: verified votes dedupe per *real* user across devices; anon votes dedupe per
device-supplied string; internal votes are unaffected.

## 6. Read path

`GET /feedback/public/{key}/posts` (existing list endpoint) gains two optional query params:

- `?voter_identity=<id>` → each returned post carries **`viewer_voted: bool`** = "has this identity
  voted for this post". Computed server-side; keyed **only** to the passed (namespaced) identity.
- `?author=<id>` → **my-submissions** filter: only posts whose `author_identity` matches.

Namespacing applies on read: an **unsigned** list request maps the passed identity into `a:`; a
**signed & verified** request maps it into `v:`. A signed GET signs per §3 with an **empty body**
(`v1 = HMAC(secret, "<t>.GET.<path>.")`); the MAC proves secret possession + binds the path, and the
`voter_identity`/`author` query values are then trusted as the verified caller's asserted identity
(same trust as the request-body identity on verified submit/vote).

**Documented residual (accepted, not fixed this round):** the signed GET does *not* cover the query
string, so a captured GET MAC permits reading `viewer_voted`/my-submissions for *arbitrary* `v:`
identities for ≤300s. Severity is low — all posts are already world-readable; the only leaked bit is
"did verified identity X vote for/submit post Y" — and the attacker model is a TLS-MITM on a
server-to-server hop. Signing the canonical query would close it and is a clean future add.

`viewer_voted` defaults to `false` when no `voter_identity` is supplied. The list stays public and
unauthenticated (portal renders it without a secret, always in `a:`).

## 7. Migration 0104

`migrations/0104_feedback_verified_identity.up.sql` / `.down.sql`.

**Columns**
- `feedback_post   ADD COLUMN identity_verified boolean NOT NULL DEFAULT false`
- `feedback_vote   ADD COLUMN identity_verified boolean NOT NULL DEFAULT false`
- `feedback_ingest_key ADD COLUMN sealed_secret text NULL`

**Backfill** — the two `UPDATE`s in §5, run after the columns are added.

**Consumed-set table** (exactly-once submit) — **locked down: RLS on, no policies, no app grants**,
so the SECURITY DEFINER functions (running as owner, which bypasses RLS) are the *sole* reader/writer.
This differs deliberately from the 0102 feedback tables, which are RLS-enabled *with* per-tenant
policies + `GRANT … TO manyforge_app` for principal access — this table has no principal-facing use,
and its `idem_key` values often embed customer-side user/order identifiers, so no principal path may
read it.
```sql
CREATE TABLE feedback_ingest_idempotency (
    key_id         uuid        NOT NULL REFERENCES feedback_ingest_key(id) ON DELETE CASCADE,
    idem_key       text        NOT NULL,          -- ALREADY tier-namespaced (a:/v:) by the DEFINER
    business_id    uuid        NOT NULL,
    tenant_root_id uuid        NOT NULL,
    body_sha256    bytea       NOT NULL,          -- of the raw submit body, for same-key-different-body → 409
    post_id        uuid        NULL,              -- backfilled after the post is created
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (key_id, idem_key)
);
ALTER TABLE feedback_ingest_idempotency ENABLE ROW LEVEL SECURITY;   -- no policies, no grants
CREATE TRIGGER feedback_ingest_idempotency_tenant_root_immutable
    BEFORE UPDATE ON feedback_ingest_idempotency
    FOR EACH ROW EXECUTE FUNCTION support_tenant_root_immutable();   -- consistency with 0102
```

**DEFINER functions** — signature/return-shape changes ⇒ **`DROP FUNCTION` + `CREATE`** (Postgres
forbids `CREATE OR REPLACE` when OUT columns or arg signatures change). Re-`REVOKE ALL … FROM PUBLIC`
+ `GRANT EXECUTE … TO manyforge_app` after **each** changed overload. All keep `SECURITY DEFINER SET
search_path = public` and stay **VOLATILE** (a `STABLE`/`IMMUTABLE` annotation would freeze the
snapshot and break the claim-first read below — pin this with a comment + source-level test).

- `feedback_public_board(p_key text)` → **also return `key_id uuid` and `sealed_secret text`** (so
  the handler can unseal in-tx and thread `p_key_id` into the consumed-set). Same oracle boundary
  (enabled key on public board only). No other callers depend on its shape (verified by grep).
- `feedback_public_submit(…, p_verified boolean, p_key_id uuid, p_idem_key text, p_body_sha256 bytea)`
  → set `identity_verified = p_verified`; store `author_identity` tier-prefixed as
  `(CASE WHEN p_verified THEN 'v:' ELSE 'a:' END) || NULLIF(btrim(coalesce(p_author_identity,'')),'')`
  (NULL author stays NULL). Identity capped `left(…, 200)` before prefixing. **Exactly-once via
  claim-first ordering** when `p_idem_key` is non-empty, all one tx (READ COMMITTED — pinned; under
  REPEATABLE READ the replay read would use a stale snapshot):
  1. Compute the namespaced key `v_ik := (CASE WHEN p_verified THEN 'v:' ELSE 'a:' END) ||
     left(p_idem_key, 255)`.
  2. `INSERT INTO feedback_ingest_idempotency (key_id, idem_key, business_id, tenant_root_id,
     body_sha256) VALUES (p_key_id, v_ik, …, p_body_sha256) ON CONFLICT (key_id, idem_key) DO
     NOTHING`; read `ROW_COUNT`.
  3. `ROW_COUNT = 0` (replay) → `SELECT post_id, body_sha256` for the pair. If stored
     `body_sha256 <> p_body_sha256` → `RAISE`/return a **409** sentinel. Else return the existing
     `post_id` with `deduped=true`. **Concurrency-safe without app retry:** because the claim INSERT
     and the post INSERT share one tx, a concurrent duplicate's `ON CONFLICT` **blocks on the PK
     lock** held by the first (uncommitted) claimant until it commits, then a fresh per-statement
     snapshot (READ COMMITTED) sees the backfilled `post_id`. (If the first tx rolls back, the
     blocked insert wins the claim and creates the post — still exactly-once.) No committed state
     ever has a NULL `post_id`.
  4. Claimed (`ROW_COUNT = 1`) → create the post, `UPDATE … SET post_id = <new>` for the pair,
     return the new id with `deduped=false`.
  When `p_idem_key` is empty/NULL, skip the consumed-set entirely (unchanged). Replay of a
  since-soft-deleted post returns the original `post_id` (`deduped=true`) — idempotency reflects the
  original operation, not current post state. Return shape `TABLE(post_id uuid, deduped boolean)`;
  the 409 is a distinct sentinel the handler maps.
- `feedback_public_vote(…, p_verified boolean)` → store `voter_identity` tier-prefixed + capped as
  above; set the vote's `identity_verified`. `ON CONFLICT (post_id, voter_identity) DO NOTHING`
  unchanged.
- `feedback_public_list_posts(p_board_id, p_limit, p_viewer text, p_author text)` → add
  `viewer_voted boolean` (via `EXISTS(SELECT 1 FROM feedback_vote v WHERE v.post_id = fp.id AND
  v.voter_identity = p_viewer)`, false when `p_viewer` NULL) and `identity_verified` to the returned
  columns; when `p_author` is non-NULL, filter `WHERE fp.author_identity = p_author`. (The handler
  passes already-namespaced `p_viewer`/`p_author`.)

**Down migration** reverses: drop the new overloads, recreate the original 0102 signatures verbatim,
strip the `a:`/`v:` identity prefixes (best-effort, see §5), drop `feedback_ingest_idempotency`, drop
the three columns.

## 8. Backend wiring (`internal/feedback`)

- `types.go` — add `IdentityVerified bool` to post/vote view types; `HasSecret bool` to the key view;
  write-once `Secret string` on the **create** response; optional `IdempotencyKey string` on the
  submit **request**; `Deduped bool` on the submit **response**; **echo `IdentityVerified` in the
  submit/vote responses** so a signing backend can assert verification happened (and detect a
  stripped-header / misconfig downgrade).
- `ingestkey.go` — `CreateIngestKey` mints+seals+returns the `fbs_` secret when a `*crypto.Sealer` is
  present; `toIngestKey` sets `HasSecret`; `ListIngestKeys` shape unchanged (now carries
  `has_secret`).
- `public.go` — signature step in `submit`/`vote`: read raw body once (reused for both the signature
  base and JSON decode + `body_sha256`), resolve board (now also selecting `key_id` + `sealed_secret`),
  apply the §3 fail-closed matrix, set `verified`, pass `p_verified` to the DEFINER; bad-sig-on-
  secret'd-key → 401 via the uniform-401 path (a `sigBad` flag, mirroring `webhook.go`). `submit`
  validates `idempotency_key` (**400 if `len > 255`** — no silent truncation) and `author/voter`
  identity (**400 if `len > 200`**), computes `body_sha256`, passes `p_key_id`/`p_idem_key`/
  `p_body_sha256`; maps `deduped` → 200 vs 201 and the 409 sentinel → 409. `list` reads
  `?voter_identity`/`?author`, namespaces per verified state, passes `p_viewer`/`p_author`. Reuse the
  existing body-size cap on the ingress group (verify it covers these routes).
- The `*crypto.Sealer` (or nil) is injected at construction (`NewPublicHandler`, service).

**sqlc:** update `db/query/feedback.sql` + `db/schema.sql` (feedback tables/functions at the tail;
add `feedback_ingest_idempotency`, the new columns, the new function params) then `make generate`
(global sqlc v1.27.0 = repo pin, safe).

## 9. Frontend (`web/`)

- **Admin key create** (`board-detail`): the returned `fbs_` secret is surfaced **once** with a
  distinct "copy it now, you won't see it again" treatment — visually and behaviourally separate from
  the always-visible `fbk_` publishable key. A `has_secret` indicator on each listed key.
- **Verified badge** on posts where `identity_verified` is true (admin moderation table).
- **Portal** (`portal`) passes `?voter_identity=<deviceId>` on its list fetch for server-truth voted
  state; the portal stays anonymous (no secret in a browser).
- `core/feedback.service.ts` / `core/public-feedback.service.ts` gain the new fields/params.

## 10. Testing

**Backend — `internal/security_regression/feedback_pins_test.go`** (extend; three-commit security
cadence — characterization → exploit → fix+invert):
- Signature: valid → verified; tampered/invalid MAC → 401; expired (`|now−t|>300s`) → 401; malformed
  header → 401; **method/path binding** — a MAC captured for one post replayed to another post → 401.
- Fail-closed: `sealed_secret` present + sealer **nil** + signature → **401** (not anon); `Sealer.Open`
  failure + signature → **401**; `sealed_secret IS NULL` + signature → **anon** (WARN). Nil-guard:
  anon submit/vote still succeed with sealer nil.
- Exactly-once: same `(key_id, idem_key, body)` → one post (second `deduped=true`, 200, same id);
  same key + **different body** → **409**; different keys → two posts; empty key → no dedupe.
- **Cross-tier idem squat regression (Fable blocking 1):** an `a:`-tier submit with idem key `X`
  does **not** collide with a `v:`-tier submit with idem key `X` (disjoint namespace → two posts,
  the verified post is never swallowed).
- Signed idempotency: flipping `idempotency_key` in a captured signed body invalidates the MAC → 401.
- Concurrent duplicate submits race to one row (PK-blocking; both observe the same `post_id`).
- **Backfill correctness (Fable blocking 2):** pre-0104 public identities become `a:`-prefixed;
  internal principal-UUID votes stay raw and **cannot** be double-voted through `post.go`'s `Vote`.
- Tier isolation: anon `v:`-raw identity lands in `a:`; verified + anon with the same raw identity →
  two distinct rows, independent dedupe.
- `viewer_voted` keyed only to the passed identity (a second identity sees `false`).
- Length caps enforced (idem_key>255 → 400; identity>200 → 400) with identical `left()` transform on
  the replay read.
- Constant-time compare present (source pin on `subtle.ConstantTimeCompare`).
- Secret never appears in `ListIngestKeys` output **and never logged** (pin); `has_secret` reflects
  presence.
- DEFINER pins: `search_path = public`, **VOLATILE**, `REVOKE … FROM PUBLIC` for every new/replaced
  func; `feedback_ingest_idempotency` has **RLS enabled, no policy, no `manyforge_app` grant**.

**Contract — `cmd/manyforge/drift_006_test.go` + `specs/006-feedback-boards/contracts/openapi.yaml`:**
optional `X-Feedback-Signature` header on submit/vote; optional `idempotency_key` (body) + `deduped`
+ `identity_verified` (response) on submit; `identity_verified` on vote response; `viewer_voted` +
`identity_verified` on the post schema; `?voter_identity` + `?author` query params on list;
`has_secret` on key-list; write-once `secret` on key-create; 409 response on submit.
`go test -tags contract ./cmd/...` must pass in the same change.

**Integration — `feedback_integration_test.go`** (`-tags integration`, testcontainers/Docker): full
signed submit+vote round-trip against real Postgres; exactly-once + 409 across two txns; concurrent
claim race; tier isolation at the `UNIQUE`; `viewer_voted` correctness; the idempotency table is
unreachable by a principal-scoped query.

**Frontend:** Vitest specs for the secret-once create UX (rendered once, `has_secret` indicator,
verified badge) + a Playwright spec for the portal voted-state round-trip. Prettier + `npm run build`
+ `npx ng test --watch=false` green. Note the pre-existing unrelated failure in
`code-review/list.spec.ts` (spec-008) — not ours, doesn't gate CI.

## 11. Verify & deploy

- Backend gates: `make test`, `make contract-test`, `make sec-test` (Docker), `make lint`,
  `make generate`.
- New migration ⇒ migrate the dev DB (currently at 103) or the app version-guard refuses to serve:
  `migrate -path migrations -database "postgres://manyforge:devpassword@localhost:55432/manyforge?sslmode=disable" up`.
- Drive the real app in a browser (admin secret-once flow + portal voted-state) before "done";
  codify as the Playwright spec above.
- PR → `master` (squash, manual merge — `--auto` races post-review commits); babysit CI +
  auto-review; watch the hub image build + Flux rollout; set `MANYFORGE_FEEDBACK_MASTER_KEY` in the
  prod sealed env before the verified tier is usable (unset is safe — anon keeps working).

## 12. Key references (templates to mirror)

- **Signed webhook path:** `internal/connectors/webhook.go` (`sigBad` flag, `Sealer.Open`, the
  unseal-fail branch at `:123-130`, known-vs-unknown oracle policy) + `migrations/0042_connector_inbound.up.sql`
  + `migrations/0043_connector_webhook_ctx.up.sql` (`connector_webhook_context` returns `sealed_secret`).
- **Sealer:** `internal/platform/crypto/sealer.go` (`NewSealer`/`Seal`/`Open`).
- **Master-key nil-guard:** `internal/platform/config/config.go` — `envKey32` (`:473`),
  `ConnectorMasterKey` doc comment (`:85-100`) — the exact semantics to copy.
- **Existing feedback DEFINERs + table RLS/CHECK/UNIQUE:** `migrations/0102_feedback_boards.up.sql`
  (functions ~131-270; tables + policies + grants ~22-128; note `feedback_post_author_chk` and
  `voter_identity` dual-use comment at :73), perms in `0103`.
- **Internal vote/submit paths that write raw principal UUIDs:** `internal/feedback/post.go`
  (`Vote`, submit) — the reason the backfill must exclude principal rows.
- **Feedback module:** `internal/feedback/{public.go,ingestkey.go,board.go,post.go,types.go}`.
- **Frontend:** `web/src/app/pages/feedback/{board-detail,portal}.ts`,
  `web/src/app/core/{feedback,public-feedback}.service.ts`.
