# Whole-tenant merge inventory and conflict decisions

**Issue:** `manyforge-cnrc.1`

**Status:** implementation gate; approved decisions for `manyforge-cnrc.2` through `.5`

**Schema baseline:** migrations `0001`–`0112`, PostgreSQL 16

## Outcome

A master-to-sub-business move is a tenant merge, not a hierarchy update. The current
schema contains 51 logical tables with a `tenant_root_id` column, 60 foreign keys whose
definition contains `tenant_root_id`, 36 root/role/owner guard triggers, and 49 RLS
policies on those logical tables. Every one of the 60 tenant-consistent foreign keys is
currently non-deferrable, so there is no valid statement ordering that can rewrite both
sides of the composite keys. The cutover migration must make those foreign keys
`DEFERRABLE INITIALLY IMMEDIATE`; only the merge primitive sets them deferred.

The authoritative inventory is PostgreSQL's migrated catalog, not a hand-maintained
production table list. The checked-in integration gate
`TestTenantMergeInventoryCoversEveryTenantRootTable` compares that catalog with an
explicit classification. Child partitions inherit the classification of their
partitioned parent and are enumerated dynamically at preflight/cutover time.

V1 is deliberately conservative:

1. It blocks every unresolved tenant-wide collision; it never renames, deduplicates, or
   chooses a winner.
2. It allows only one human principal who is a direct built-in Owner of both roots.
   Different owners and dual approval are future work.
3. It drains active external work, rewrites safe pending work, and prevents new claims
   while the roots are fenced.
4. It pre-stages attachment objects under destination-root keys before the database
   transaction. No other current integration requires external re-registration.
5. It remains a single database transaction inside the bounded V1 capacity envelope.

## Classification vocabulary

| Code | Meaning |
|---|---|
| `R` | Rewrite `tenant_root_id` from source root `S` to destination root `D` in the cutover transaction. |
| `H` | Rebuild derived hierarchy data; a root rewrite alone is insufficient. |
| `C` | Run an explicit collision query. Any hit blocks V1 confirmation. |
| `F` | Fence new writes/claims and drain an already-active transaction or lease before cutover. |
| `X` | Pre-stage an external resource before cutover and clean up the old resource after commit. |
| `N` | The root is nullable; rewrite only source-owned non-null rows and preserve global/null rows. |
| `A` | Normally immutable history receives a narrowly-scoped ownership-metadata rewrite. |

All database rows below are changed in one transaction. Unless a row says otherwise,
rollback means transaction rollback restores the original root, constraints, hierarchy,
and visibility. Verification always includes `tenant_root_id = S` returning zero for the
listed source-owned rows, constraint validation, and the module-specific query in the
last column.

## Authoritative table inventory

Every object listed in an Objects cell inherits all rules in that row.

| Owner | Objects | Class | Migration / reconciliation rule | Conflict rule | Fence / external behavior | Module verification |
|---|---|---:|---|---|---|---|
| Tenancy | `business` | `R H F` | Rewrite the complete source subtree to `D`; set `S.parent_id = P`; preserve every business ID and relative source edge. The privileged merge guard is the only path allowed through `business_root_guard`. | Block inactive/deleting roots, cycles, and a resulting depth over 10. | Exclusive locks on `S` and `D` drain hierarchy writers. | Every source business has root `D`; `S.parent_id=P`; source parent relations are unchanged. |
| Tenancy | `business_closure` | `R H` | Rewrite all existing source-internal closure rows to `D`, then add every `(destination ancestor of P, source descendant)` path with `depth(A,P)+1+depth(S,X)`. | The composite PK must have no duplicate generated pair. | Covered by root locks. | Exactly one self row per business; no cycles; closure equals a recursive traversal; max depth ≤10. |
| Identity | `principal` | `N R F` | Human principals remain global with null root/home business. Rewrite source agent principals to `D`; keep `home_business_id`. | IDs are globally unique. Block any source agent whose home business is not in the source subtree. | Drain running agent work first. | Every agent's `(home_business_id,tenant_root_id)` resolves to a moved business; humans are byte-for-byte unchanged. |
| IAM | `role` | `N R C` | Preserve preset roles (`tenant_root_id IS NULL`). Rewrite source custom roles to `D`. | Block duplicate custom `(D,key)`; no rename or permission merge. | Root write fence. | Every moved custom role points to `D`; `role_permission` rows and role IDs are unchanged. |
| IAM | `membership`, `invitation` | `R F` | Rewrite root only; preserve business, principal, role, grant, token, and status fields. Source-root Owner membership becomes an Owner grant scoped to the `S` subtree. | Business IDs make membership uniqueness stable. Invitation custom-role IDs must refer to a moved or preset role. | Drain invitation accept and membership mutations. | Source members can reach `S` and its descendants, not `P`, `D`, ancestors, or siblings; destination Owner remains valid. |
| Support identity | `email_domain`, `inbound_address`, `requester` | `R C F` | Rewrite root and preserve IDs, routing tokens, DNS/DKIM material, and business association. | Block duplicate domain, address, or normalized requester email in `D`. No automatic identity merge. | Fence inbound SMTP/webhook resolution and support writes. DNS/webhook endpoints keep stable IDs and need no re-registration. | All routing lookups resolve the same business after commit; no source-root route remains. |
| Support | `ticket`, `ticket_tag`, `ticket_message` | `R C F` | Rewrite root only; preserve threading, connector links, delivery state, IDs, tags, and history. | Block duplicate reply token, RFC message ID, or any composite FK mismatch in `D`. | Fence ticket writes and drain active outbox subscribers. Safe pending mail/events are rewritten and delivered after commit. | Ticket/message/tag counts and ID digests match preflight; all composite links validate. |
| Blob / support | `attachment` | `R C F X` | Before cutover copy each live object from `S/business/ticket/attachment` to `D/business/ticket/attachment`; in the transaction rewrite both root and `blob_key`. | Block a destination key collision or missing/size-mismatched source object. | Drain `attachment.purge` first. After commit delete old objects from the immutable manifest; on pre-commit failure delete only staged copies. | New object exists with matching size/checksum for every live row; no DB row references an old-root key. |
| CRM | `company`, `contact`, `activity_entry` | `R C F` | Rewrite root only; preserve IDs, soft-delete state, business attribution, source IDs, and history. | Block active company-domain, active contact-email, or activity dedup collisions. No contact/company coalescing. | Fence CRM and principal-less inbox activity writes. | Counts and ID digests match; requester/contact/company composite links validate. |
| Agents | `agent`, `agent_run`, `approval_item` | `R F` | Rewrite root while preserving agent principal, target IDs, progress, cost, and approval arguments. | Business-scoped agent names remain safe because business IDs do not change. | Block `agent_run.status='running'`; allow queued/terminal rows after claims are fenced. Drain claimed approval events. | No running source job at cutover; every moved agent/run/approval resolves within `D`. |
| AI / MCP | `ai_provider_credential`, `codex_oauth_pending`, `mcp_server`, `mcp_tool_policy` | `R F` | Rewrite root only. AES-GCM ciphertext has no root-bound associated data, so no re-encryption is needed. | Business-scoped provider/name uniqueness is stable. Block malformed cross-root references. | Fence token refresh, OAuth completion, MCP calls, and credential writes. Pending OAuth may continue after rewrite. | Secrets open with the existing master key; policies and pending flows resolve the same business. |
| Secrets / connectors | `secret`, `connector`, `connector_sync_state`, `connector_webhook_delivery`, `connector_outbound_op` | `R F` | Rewrite root only; preserve connector IDs, URLs, sealed credentials, external IDs, snapshots, attempts, and leases. | Business-scoped connector uniqueness is stable. Block inconsistent source references. | Fence reconcile/webhook claims. Block/drain `connector_outbound_op.status='in_progress'`; pending ops move safely. | Pending/terminal counts and IDs match; connector/secret/ticket links validate; no in-progress source op exists at commit. |
| Repositories / review | `repo_connector`, `code_review`, `code_review_finding_seen`, `review_config`, `review_dimension`, `review_dimension_repo_override` | `R F` | Rewrite root only; preserve repo, PR, finding, configuration, progress, and lease data. | Business/repository uniqueness is stable because business IDs do not change. | Block/drain running reviews and their Kubernetes sandbox jobs; fence claims, lease renewal, webhook ingest, and enqueue. Pending reviews move safely. | No running source review at cutover; every review/config reference resolves inside `D`. |
| GitHub App | `github_app_installation` | `N R F` | Preserve unlinked/null installations. Rewrite linked source installations; keep installation ID, business, agent, and config. | `installation_id` is globally unique already; inconsistent business/root/agent links block. | Fence GitHub webhook ingestion and installation updates. The GitHub installation and callback state use stable IDs, so no re-registration is required. | `github_installation_context` returns `D` and the same business/agent for each moved installation. |
| Feedback | `feedback_board`, `feedback_post`, `feedback_vote`, `feedback_ingest_key`, `feedback_ingest_idempotency` | `R F` | Rewrite root only; preserve board/post/vote IDs, public keys, sealed secrets, idempotency results, and ticket links. | Business/post/key-based uniqueness remains stable. Any malformed cross-root link blocks. | Fence public ingestion and admin writes; wait for active ingest transactions. Public keys remain valid. | Counts, IDs, vote totals, and key/idempotency links match; public lookup resolves `D`. |
| Telemetry ingest | `telemetry_client`, `analytics_event`, `crash_event` | `R F` | Rewrite root on logical parents so PostgreSQL routes updates through every current child partition. Preserve public keys, client IDs, event IDs, timestamps, and payloads. | Client/event IDs and publishable keys are globally unique. Block inconsistent business/client scope. | Fence public collection. Lock source clients `FOR UPDATE`, which drains existing `FOR SHARE` ingests. Lock partition maintenance during enumeration. | Per-partition counts and `(ingested_at,id)` digests match; every event client/business resolves in `D`. |
| Telemetry rollups | `analytics_event_daily`, `analytics_daily`, `analytics_page_daily`, `analytics_referrer_daily`, `analytics_dimension_daily` | `R F` | Rewrite root only; preserve client, business, bucket, dimension, and aggregate values. | Root-keyed aggregate uniqueness is collision-free when client IDs are globally unique; still preflight the exact destination keys. | Acquire rollup advisory locks in existing worker order and drain active sweeps. | Aggregate row counts and value sums match; recomputation from raw events produces the same rows. |
| Audit | `audit_entry` | `N R A` | Preserve global/null audit rows. For source-owned entries rewrite only `tenant_root_id` to `D`; never change action, actor, target, values, decision, correlation, or timestamp. Append two merge-manifest audit rows, attributed to businesses `S` and `D`, under root `D`. | No tenant-wide unique key. A non-null source audit business outside the source subtree blocks. | Root fence. | Content digests excluding `tenant_root_id` are identical; two manifest rows share the operation ID. |
| Platform events | `outbox` | `R F` | Rewrite the envelope root. Rewrite embedded `tenant_root_id` only for `business.created` and `agent.action.approved`; all other payload IDs remain stable. | Unknown topics or malformed payloads block confirmation instead of being silently moved. | Drain claimed rows. Pending safe topics move; `attachment.purge` must drain before blob staging. | Topic/payload counts and IDs match; no pending or processed row retains `S` in envelope or known scope fields. |
| Notifications | `notification` | `R` | Rewrite root only; preserve principal, kind, ref, read state, and time. | Principal IDs are global; malformed scope-bearing refs block. | Root write fence. | Counts/IDs match and each principal can read the same notification after commit. |

## Indirect and non-column scope inventory

These objects do not carry a `tenant_root_id` column but were checked explicitly.

| Owner / object | Classification and rule | Verification |
|---|---|---|
| IAM `role_permission` | Derived from stable `role.id`; no row rewrite. It follows moved custom roles automatically. | Every moved custom role retains the same permission-key set. |
| Auth `account`, `refresh_token`, `one_time_token`, `account_erasure` | Global human-account state; unaffected. Agent principals cannot authenticate through these tables. | Content digest unchanged. |
| Mail `email_suppression` | Global address suppression; unaffected by organizational ownership. | Content digest unchanged. |
| GitHub `github_webhook_delivery`, `github_setup_nonce` | Global installation/delivery or single-use nonce state; stable installation/business IDs make rewrite unnecessary. Signed state contains `business_id`, not a root ID. | Existing delivery dedup and nonce behavior remains unchanged. |
| Telemetry `analytics_salt`, `rollup_state`, `partitioned_table` | Global privacy/worker metadata; no row rewrite. Their workers are fenced/locked during cutover. | Watermarks, salts, and partition policy rows are unchanged. |
| Global `permission`, preset `role`, `model_pricing`, `github_app_config` | Installation-wide catalogs/configuration; unaffected. | Content digest unchanged. |
| JSON scope fields | Known root-bearing payloads are `outbox.payload.tenant_root_id` for `business.created` and `agent.action.approved`. Other JSON (`approval_item.args`, audit values, connector/repo config, notification refs, review findings/progress, analytics/crash payloads) carries stable resource IDs or opaque user data and is not recursively rewritten. | Preflight validates known topic schemas and scans exact JSON key `tenant_root_id`; an unknown occurrence blocks. |
| In-process caches | OpenRouter/Hugging Face catalogs are global; Codex models are keyed by ChatGPT account ID; rate-limit buckets use IP/public key. No authorization or hierarchy cache exists. No invalidation is required. | After cutover, live lookups resolve scope from the database; cache keys remain stable. |
| Review sandbox resources | Kubernetes Jobs, local workdirs, and progress streams are transient children of a running `code_review`. They are not moved. | Preflight requires no running source review/job. |
| Object storage | Attachment keys embed the root and are the only current external resource whose identity changes. | Copy/checksum/switch/cleanup protocol in the table matrix. |

## Constraint, trigger, and RLS implications

Catalog baseline:

- 60 foreign keys include `tenant_root_id`; all 60 are non-deferrable.
- 32 tables at migration 110 use `support_tenant_root_immutable`; migration 112
  replaces the telemetry client instance with an owner-aware guard. Along with
  `business_root_guard`, the membership role/agent guards, and the deferred last-Owner
  guard, there are 36 relevant triggers.
- 49 RLS policies cover logical tenant-root tables. `principal` is intentionally
  global; some principal-less ingest/idempotency tables are protected by grants and
  SECURITY DEFINER functions instead.

Implementation requirements:

1. A migration changes all catalog-discovered tenant-consistent foreign keys to
   `DEFERRABLE INITIALLY IMMEDIATE`. Normal transactions retain immediate checking.
2. The merge transaction executes `SET CONSTRAINTS ALL DEFERRED`.
3. Direct app updates remain unable to change roots. A generalized guard permits a
   root change only when `current_user` owns the table and a transaction-local merge
   operation marker resolves to the currently-running operation for `S` and `D`.
   A caller-set GUC alone is insufficient because direct statements still run as
   `manyforge_app`.
4. The merge primitive is SECURITY DEFINER, pins `search_path`, is executable only by
   `manyforge_app`, rechecks authorization internally, and updates catalog-discovered
   tables only after matching them to the checked-in classification.
5. RLS remains enabled. The table owner performs the privileged rewrite; ordinary
   readers see either the pre-commit or post-commit snapshot, never partial state.

## Collision matrix

| Key / invariant | V1 decision |
|---|---|
| `role (tenant_root_id,key)` | Block duplicate custom keys. User must rename/delete one role before retrying. |
| `company (tenant_root_id,domain)` | Block duplicate non-null domains. No company merge. |
| `contact (tenant_root_id,primary_email) WHERE deleted_at IS NULL` | Block duplicate active email. No contact merge. |
| `requester (tenant_root_id,email)` | Block duplicate normalized email. No requester merge. |
| `email_domain (tenant_root_id,domain)` | Block duplicate domain. DNS ownership does not select a winner. |
| `inbound_address (tenant_root_id,address)` | Block duplicate route. No automatic rerouting. |
| `ticket (tenant_root_id,reply_token)` | Block an exact token collision and require token rotation before retry. |
| `ticket_message (tenant_root_id,message_id)` | Block an exact RFC message-ID collision. |
| `activity_entry (tenant_root_id,source_type,source_id,kind)` | Block a dedup-key collision. |
| `attachment (tenant_root_id,blob_key)` and destination object key | Block an existing destination row/object or checksum mismatch. |
| Root-keyed analytics aggregates | Block an exact destination key collision, although globally unique client IDs make a legitimate collision unreachable. |
| `codex_oauth_pending (jti,tenant_root_id)` | `jti` is also the global primary key, so root rewrite cannot collide; malformed duplicates still block. |
| Business-scoped unique keys | Safe because every business ID is preserved and globally unique. |
| UUID primary keys, publishable keys, invitation tokens, GitHub installation IDs | Already global uniqueness boundaries; preserve unchanged. |

Preflight reports every conflict, not merely the first one. Confirmation is impossible
until a new preflight returns zero blockers.

## Write and worker fencing

Every tenant write path, including SECURITY DEFINER ingestion, must take a
transaction-scoped **shared** advisory lock for its root through a common write-guard
trigger/helper. The merge takes **exclusive** locks for `S` and `D` in UUID byte order.
This closes the absent-row race that a fence table alone would have: the exclusive lock
waits for earlier writers and blocks later writers before final revalidation. Existing
hierarchy writers already use the compatible exclusive tenant lock.

| Path | Before cutover | Pending-work policy |
|---|---|---|
| Ordinary API writes, membership/invitation acceptance, support/CRM writes | Shared root lock plus active-operation check; later writes return a retryable `TENANT_MERGE_IN_PROGRESS`. | No queued state. |
| Outbox claim/dispatch | Claim skips fenced roots and holds the shared root lock through handler commit. | Rewrite safe unclaimed rows. Drain `attachment.purge`; validate known payload schemas. |
| Inbound SMTP/webhook and support DEFINER functions | Resolve the root, take shared lock, then revalidate the route before insert. | Request retries after the fence. |
| Feedback and telemetry public ingest | Key lookup takes shared root lock. Merge locks ingest-key/client rows and revalidates after waiting. | Request returns retryable/unobservable ingest response as appropriate. |
| Connector webhook/reconcile/inbound sync | Claims and DEFINER writes take shared root lock. | Pending sync events move; active handler transactions drain. |
| Connector outbound dispatcher | Claim takes shared root lock. | Pending ops move; in-progress ops must settle or reach terminal/retriable pending state. |
| Agent run drainer/reaper and approval expiry/executor | Claim/update functions skip fenced roots and hold shared locks. | Queued/awaiting rows move; running runs and claimed approvals drain. |
| Code-review worker/lease heartbeat/GitHub webhook | Claim, renew, enqueue, and terminal writes take shared locks. | Pending reviews move; running review and sandbox job must finish or be terminally canceled before confirmation. |
| Codex refresh/OAuth completion | Claim/update takes shared root lock. | Pending OAuth row moves; active token refresh/callback drains. |
| Analytics rollups | Merge acquires `rollup_analytics_daily`, `rollup_analytics_pageviews`, and `rollup_analytics_dimensions` locks in existing order. | Sweep completes first; aggregates are rewritten after it drains. |
| Partition maintenance | Merge acquires `partition_maintenance` while enumerating/updating partition parents. | No tenant data queue; policy/watermarks remain global. |

## Capacity and timing envelope

V1 is enabled only for a preflight satisfying all of these limits:

| Dimension | Hard V1 limit |
|---|---:|
| Businesses in source subtree | 1,000 |
| Resulting hierarchy depth | 10 |
| Source-owned relational rows, including raw-event partitions | 250,000 |
| Sum of `pg_column_size(row)` over source-owned rows | 1 GiB |
| Live attachment objects | 10,000 |
| Live attachment bytes to pre-stage | 1 GiB |
| Exclusive root-lock acquisition | 10 seconds |
| Database cutover statement timeout | 60 seconds |
| Required release-gate p95 at the maximum synthetic envelope | 30 seconds |

The preflight and blob-copy phases are asynchronous and outside the exclusive-lock
window. `manyforge-cnrc.8` must prove the 250,000-row/1-GiB envelope, 30-second p95,
rollback, WAL/storage headroom, and two-replica worker behavior before the feature flag
is enabled. A source above any limit receives a `capacity_exceeded` blocker and requires
a separately-designed offline migration; V1 does not silently attempt an oversized
transaction.

## Authorization and approval decision

V1 requires all of the following at confirmation and again inside the merge primitive:

1. The actor is one human principal with direct membership in both `S` and `D`.
2. Both direct memberships use the built-in locked Owner role; inherited or custom-role
   equivalence is insufficient.
3. The actor has `hierarchy.manage` at destination parent `P`.
4. Authentication age is at most 10 minutes.
5. The actor types the exact source business name and destination business name and
   submits the preflight operation ID/generation.

Different source and destination owners do not trigger a dual-approval workflow in V1;
the move is ineligible unless one person independently owns both. Dual approval can be
added later without weakening this rule.

## Preflight generation and cutover order

Preflight runs from a repeatable-read snapshot and stores:

- schema migration version and inventory version;
- source/destination/parent lifecycle and authorization facts;
- per-object row count, logical bytes, stable-ID digest, and conflict-key digest;
- partition names/bounds, worker-state counts, unknown outbox topics/JSON scope keys;
- attachment source/destination keys, size, and checksum;
- every blocker and the capacity result.

Confirmation performs these steps:

1. Verify typed confirmation, fresh auth, operation state, and inventory/schema version.
2. Verify all attachment copies are staged and checksummed.
3. Acquire exclusive `S`/`D` locks in UUID order plus rollup/partition locks.
4. Mark the durable operation running, fence claims, and wait for active shared-lock
   holders to finish.
5. Recompute the preflight fingerprints and blockers under the locks. Any difference
   invalidates confirmation and returns to `preflight_required`.
6. Set the privileged operation marker and defer all tenant-consistent constraints.
7. Rewrite leaf/module rows, known JSON scope fields, custom roles, agent principals,
   memberships, audit scope, and attachment keys.
8. Rewrite the business subtree, rebuild closure, and revalidate owner/agent/role
   invariants.
9. Assert the source residue query is empty, validate constraints, append the immutable
   manifest/audit rows, and mark the operation succeeded.
10. Commit once. Release locks, invalidate no caches, and asynchronously delete
    manifest-listed old attachment objects.

Failure before step 10 rolls the database back completely. Pre-staged destination
objects are unreferenced and can be safely removed. V1 makes no automatic post-commit
reversal promise because new destination writes may immediately intermingle.

## Catalog and regression mechanism

The integration test derives logical tenant-root tables with:

```sql
SELECT c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a
  ON a.attrelid = c.oid
 AND a.attname = 'tenant_root_id'
 AND NOT a.attisdropped
WHERE n.nspname = 'public'
  AND c.relkind IN ('r', 'p')
  AND NOT EXISTS (
      SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid
  )
ORDER BY c.relname;
```

CI fails if the migrated catalog and checked-in classification differ in either
direction. Future preflight code must use catalog queries for columns, partitions,
foreign keys, unique indexes, triggers, and policies, then join those results to the
same versioned classification. It must fail closed on:

- an unclassified tenant-root table or child partition parent;
- a new JSON `tenant_root_id` occurrence without a topic/column rule;
- an unknown outbox topic;
- a source-owned row reachable only through an unclassified indirect reference;
- a tenant-consistent constraint that the merge transaction cannot defer.

This makes schema growth an explicit tenant-merge design decision rather than a silent
security-boundary omission.
