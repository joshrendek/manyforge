# Mailing Lists & Drip Automations — Design + Implementation Plan

Specs: `013-mailing-lists` (lists, subscribers, sending profiles, broadcast campaigns, tracking, provider webhooks) and `014-automations` (branching drip automations on an auto-layout canvas). 014 builds on 013 and lands second.

## 1. Context

manyforge tenants (businesses) have no way to email their own audiences. The platform already owns a large share of the plumbing — `notify.Sender`/`Mail`, `SMTPSender` with DKIM, per-tenant verified `email_domain`, a global `email_suppression` table, an HMAC bounce intake, a secrets vault + connector pattern, an outbox worker and lease-based work queues, and CRM contacts with an activity timeline — but has **no email templating, no per-tenant suppression, no unsubscribe plumbing, and no bulk/scheduled sending**. `docs/ROADMAP.md` line SL-D ("templated outbound … digesting") lists exactly this gap as planned-but-unbuilt.

garden.gg (the first real consumer) uses **Resend**, not SES (`~/dev/garden.gg/internal/email/resend.go`, `RESEND_API_KEY`), and sends a single transactional email with no deliverability plumbing. Its `Sender` interface / terminal-vs-transient error split is the seam we mirror.

Outcome: a business can connect its own Resend/SES credentials **or** send through the platform relay with its verified domain; build lists (public form, server-to-server API, CSV import, from CRM contacts) with per-list double opt-in; send Markdown-authored broadcasts with click/open tracking, bounce/complaint handling, and per-tenant suppression; and run branching drip automations (send / wait / condition / tag / exit) edited on an auto-layout canvas.

## 2. Decisions (locked with the user)

| Topic | Decision |
|---|---|
| Sender model | Both: BYO provider per business **and** shared platform relay |
| Providers v1 | Resend (API key) + Amazon SES (sesv2, access key/secret/region) + platform relay (existing `SMTPSender` + tenant `email_domain` DKIM) |
| Acquisition | Public subscribe endpoint + hosted/embeddable form; S2S HMAC API; CSV import (sync, 5 MiB / 50k rows); add from CRM contacts |
| Consent | Per-list `double_opt_in` (default ON). Non-public paths may skip confirmation but must record `consent_source` + `consent_attested_by` |
| Drip shape | Branching automations (DAG from one trigger); node kinds trigger / send_email / wait / condition / add_tag / remove_tag / exit |
| Builder UI | **Auto-layout canvas** — top-down layered layout, `+` on edges, side-panel node editor, pan/zoom, keyboard nav; no node dragging, no stored x/y |
| Tracking | Clicks (signed redirect) + opens (pixel), per-campaign/per-node toggles, default ON |
| Authoring | Markdown → server-rendered HTML in a shared responsive layout + generated plain text; `{{first_name}}`-style variables; goldmark with raw HTML disabled (no bluemonday needed) |
| Shared-relay identity | Reuse existing `email_domain` verification; nothing new for identity |
| Postal address | Optional on profile; footer omits when blank; UI warning banner |
| SES duplicate policy | Accept at-least-once on crash-after-accept (Resend gets `Idempotency-Key` = delivery id) |
| Relay bounces | Tenant-scoped suppression when the Message-ID matches a `mailing_delivery`, else today's global behaviour |
| Confirm/unsubscribe pages | GET renders a button, only POST mutates (scanner-safe); RFC 8058 one-click POST also honoured |
| Spec split | `specs/013-mailing-lists/` then `specs/014-automations/`, each with `contracts/openapi.yaml` + `drift_NNN_test.go` |

## 3. Architecture overview

```
                 tenant site / garden.gg backend            mail clients
                 ┌──────────────┐  ┌────────────────┐        ┌──────────┐
                 │ hosted form  │  │ S2S HMAC (mls_)│        │ /m/c /m/o│ click/open
                 │ /m/s/{key}   │  │ subscribers,   │        │ /m/u     │ unsubscribe (GET btn / POST / one-click)
                 └──────┬───────┘  │ events         │        │ /m/confirm│
                        │          └───────┬────────┘        └────┬─────┘
   ingress group (per-IP bucket, no-oracle, DEFINER-only) ────────┘
                        │
   ┌────────────────────▼───────────────────────────────────────────────────┐
   │ internal/mailing  (spec 013)                                            │
   │  lists · subscribers · tags · keys · suppression · templates            │
   │  sending profile ──► provider/ {relay | resend | ses}  (Deliverer)      │
   │  campaigns ─fan-out─► mailing_delivery ◄─enqueue─ automations (014)     │
   │  render/ (goldmark → layout.html → link rewrite → pixel → text)         │
   │  sendworker: claim(lease) → render → Deliverer.Send → record            │
   │  webhooks: /inbound/mailing/{profile}/{ses|resend} → apply event        │
   │  outbox topics: mailing.subscriber.{activated,tag_added,status_changed} │
   └────────────────────┬───────────────────────────────────────────────────┘
                        │ ports (SubscriberReader, MessageSender.Enqueue, EngagementReader, Tagger)
   ┌────────────────────▼───────────────────────────────────────────────────┐
   │ internal/automations (spec 014)                                         │
   │  automation · automation_version(graph jsonb) · enrollment · step      │
   │  Stepper: claim due enrollments (lease) → Advance() in ONE tx           │
   │  outbox subscribers: enroll on trigger, exit on status change           │
   └─────────────────────────────────────────────────────────────────────────┘
```

Why this shape (alternatives rejected): a job library (river/asynq) breaks the repo's hand-rolled poller + outbox convention; delegating to provider-native broadcasts/contact lists leaks provider specifics through the abstraction and can't do branching. Everything above reuses an existing pattern.

## 4. Cross-cutting conventions (every slice)

Discovered during planning; non-negotiable for any new table/route:

- **Tenant tables**: `(business_id, tenant_root_id)` composite FK to `business`, `UNIQUE (id, tenant_root_id)`, RLS `authorized_businesses(current_principal())` on USING **and** WITH CHECK, `support_tenant_root_immutable` trigger, **`tenant_merge_write_fence` trigger + `tenant_merge_manifest` INSERT** (`drain_fence_then_rewrite`; pattern `migrations/0122_analytics_property_governance.up.sql:58-72`), GRANT to `manyforge_app`, mirror in hand-maintained `db/schema.sql`. Register every table + `*_tenant_root_id_fkey` in `internal/security_regression/tenant_merge_inventory_test.go`.
- **Principal-less paths** (workers, webhooks, public ingress) cannot `ON CONFLICT` on RLS tables (`internal/inbox/provision.go:85-95`) → all such reads/writes go through `SECURITY DEFINER SET search_path = public` functions, `REVOKE ALL FROM PUBLIC; GRANT EXECUTE TO manyforge_app`. Claim functions filter on `tenant_merge_root_write_allowed(tenant_root_id)`.
- **Services**: `s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx))`, `resolveTenantRoot`, second `tenant_root_id = $n` predicate in every id-taking query ("dual enforcement", `db/query/feedback.sql` header), `errs.ErrNotFound/ErrValidation/ErrConflict`, foreign == unknown == 404. Reference trio: `db/query/feedback.sql`, `internal/feedback/board.go`, `internal/feedback/handler.go`.
- **Permissions** seeded by migration like `migrations/0103_feedback_permissions.up.sql`; constants in `internal/authz/perms.go` and `internal/security_regression/perm_constants_pin_test.go`.
- **Routes** mounted in `cmd/manyforge/main.go mountAPIRoutes` (~L1125) under `httpx.RequirePermission` groups; public ones in the ingress group (~L1148-1176, `h.ingestLimit`); root-mounted tracking next to `analyticsPublic`. Add to `testHandlers()` and a `drift_NNN_test.go` (`//go:build contract`, both directions).
- **Enum additions**: new values can't be used in the migration that adds them (`migrations/0047`).
- **Security rules** from global CLAUDE.md apply (no `err.Error()` to clients, `hmac.Equal`, `netsafe.NewClient` for all outbound HTTP, token redaction in logs, `http.MaxBytesReader`, COALESCE/NULLIF PATCH semantics).
- sqlc is pinned v1.27.0 (`make generate`); `go test -tags contract ./cmd/...` and staticcheck run in `make lint`/`make check`.

## 5. Spec 013 — mailing lists (backend)

### 5.1 Package layout

```
internal/mailing/
  service.go          Service{DB, Vault *secrets.Vault, Sealer, DKIMSealer, PublicBaseURL, Rand, Clock}
  lists.go subscribers.go tags.go keys.go profile.go suppression.go templates.go campaigns.go import.go
  handler.go          ReadRoutes / WriteRoutes / SendRoutes (chi); thin
  public.go           subscribe (JSON + form POST), S2S subscribers/events (principal-less, DEFINER-only)
  track.go            /m/c /m/o /m/u /m/confirm (root-mounted, ingestLimit inside handler like analytics)
  token/codec.go      HMAC tokens (HKDF purposes)
  render/             render.go, templates/layout.html (embed), text.go, testdata/ goldens
  provider/           deliverer.go (Deliverer + Classify), relay.go, resend.go, ses.go, cache.go
  webhook_ses.go webhook_resend.go   snsverify/verify.go
  sendworker.go       fan-out + delivery dispatcher (mirrors internal/connectors/outbound.go)
  ports014.go         implements automations.{SubscriberReader,MessageSender,EngagementReader,Tagger,TemplateReader,ListReader} (added in 014 slices)
```

Providers live under `internal/mailing/provider/` (not `platform/notify`, which must stay AWS-SDK-free). `notify.Sender.Send` returns only `error`, so mailing defines:

```go
type Deliverer interface { Send(ctx context.Context, m notify.Mail) (SendResult, error) }
type SendResult struct { ProviderMessageID string } // relay: our own Message-ID
```

Export `notify.BuildMIME(Mail) ([]byte, error)` (wrapping the existing `buildMIME` in `internal/platform/notify/smtp.go`) so SES raw sends reuse the CR/LF-rejecting header chokepoint.

### 5.2 Data model

**0124_mailing_core** — enums `mailing_send_mode('relay','resend','ses')`, `mailing_subscriber_status('pending','active','unsubscribed','bounced','complained')`, `mailing_consent_source('public_form','api','csv_import','crm','manual')`, `mailing_suppression_reason('bounce','complaint','unsubscribe','manual')`.

- `mailing_sending_profile` — `UNIQUE (business_id)` (one per business in v1); `mode`, `from_email citext`, `from_name`, `reply_to`, `postal_address NULL`, `email_domain_id NULL` (FK `(email_domain_id, tenant_root_id)→email_domain`), `secret_ref NULL` (FK→`secret`, credential JSON sealed with the mailing sealer via `Vault.Put(tx, businessID, "mailing", …)`), `ses_region`, `ses_configuration_set`, `sns_topic_arn`, `status('unverified','verified','error')`, `last_verified_at`, `verify_error`. CHECK: `relay ⇒ email_domain_id NOT NULL`, `resend|ses ⇒ secret_ref NOT NULL`. Resend `webhook_secret` lives in the same sealed blob.
- `mailing_list` — `slug`, `name`, `description`, `double_opt_in bool DEFAULT true`, `status('active','archived')`; `UNIQUE (business_id, slug)`.
- `mailing_list_key` — `list_id`, `publishable_key text UNIQUE` (`mlk_`+32 b64url), `sealed_secret NULL` (`mls_` S2S secret, sealed), `label`, `status('enabled','revoked')`, `revoked_at`.
- `list_subscriber` — `list_id`, `email citext`, `first_name`, `last_name`, `attributes jsonb DEFAULT '{}'`, `status`, `contact_id NULL` (FK `(contact_id, tenant_root_id)→contact`), `consent_source`, `consent_attested_by NULL → principal`, `consent_ip inet`, `consent_user_agent`, `consent_at`, `confirm_token_hash bytea NULL`, `confirm_expires_at`, `confirmed_at`, `unsubscribed_at`, `status_reason`. `UNIQUE (list_id, email)`; indexes `(list_id, status, id)`, partial `(confirm_token_hash)`, `(business_id, email)`. CHECK `consent_source='public_form' OR consent_attested_by IS NOT NULL`.
- `subscriber_tag` — `list_id`, `subscriber_id`, `tag`; `UNIQUE (subscriber_id, tag)`; index `(list_id, tag, subscriber_id)`.
- `mailing_suppression` — `email citext`, `reason`, `source`; `UNIQUE (business_id, email)`.
- `mailing_template` — `name`, `subject`, `preheader`, `body_markdown`, `track_opens`, `track_clicks`; reusable content referenced by 014 `send_email` nodes (edits affect future sends — documented, standard ESP behaviour).
- permissions: `mailing.read` (owner/admin/member/viewer), `mailing.write` (owner/admin), `mailing.send` (owner/admin — separated because a blast is the high-blast-radius action). Constants `authz.PermMailingRead/Write/Send`.

**0125_mailing_public_definers** — `mailing_public_list(p_key)` (enabled key on active list only → `(list_id, business_id, tenant_root_id, double_opt_in, key_id, sealed_secret)`, zero rows otherwise = the oracle boundary); `mailing_public_subscribe(...)` (ON CONFLICT `(list_id,email)`: reactivates `unsubscribed`→`pending|active`, never `bounced|complained`; returns `(subscriber_id, created, status)`); `mailing_confirm(p_token_hash)` (activates if unexpired, clears hash, returns row count — caller renders the same page either way); `mailing_unsubscribe(p_subscriber_id, p_campaign_id, p_reason)` (status + `mailing_suppression('unsubscribe')` + tracking event); `mailing_record_track(p_delivery_id, p_kind, p_url, p_ip, p_ua)`; `mailing_s2s_unsubscribe(list, email)`; `mailing_relay_identity(p_email_domain_id)` (verified domain + sealed DKIM ref, same predicate as `get_send_identity` in 0023).

**0126_mailing_campaigns** — enums `campaign_status('draft','scheduled','sending','sent','cancelled','failed')`, `mailing_delivery_status('queued','sending','sent','delivered','bounced','complained','failed','suppressed','cancelled')`, `mailing_track_kind('open','click','unsubscribe','delivered','bounce','complaint')`, `mailing_delivery_source('campaign','automation')`.

- `campaign` — `list_id`, `profile_id NULL`, `name`, `subject`, `preheader`, `body_markdown`, `tag_filter text[] DEFAULT '{}'`, `track_opens`, `track_clicks`, `status`, `scheduled_at`, `started_at`, `completed_at`, `fanout_cursor uuid NULL`, `fanout_done bool`, counters `recipient/sent/delivered/bounced/complained/opened/clicked/unsubscribed/failed_count`, `last_error`, `created_by`. Partial indexes `(scheduled_at) WHERE status='scheduled'`, `(status) WHERE status='sending'`.
- `mailing_delivery` (shared by campaigns, automations, and 014's port) — `source_kind`, `source_id uuid` (campaign_id | automation step id), `campaign_id NULL`, `template_id NULL` (CHECK exactly one content source), `subscriber_id`, `email citext`, `status`, `attempts`, `not_before`, `lease_until NULL`, `message_id text UNIQUE` (`{id}@{message_domain}`), `provider_message_id NULL`, `opened_at`, `first_clicked_at`, `last_error`. `UNIQUE (source_kind, source_id, subscriber_id)` (campaign dedupe **and** 014 replay idempotency). Indexes: `(not_before, created_at) WHERE status='queued'`, `(lease_until) WHERE status='sending'`, partial `(provider_message_id)`, `(campaign_id, status)`.
- `mailing_tracking_event` — `campaign_id NULL`, `delivery_id NULL`, `subscriber_id NULL`, `kind`, `url`, `ip`, `user_agent`, `provider_payload jsonb`, `occurred_at`. Indexes `(campaign_id, kind, occurred_at DESC)`, `(delivery_id, kind)`.
- `mailing_provider_webhook_delivery` — `profile_id`, `provider`, `external_event_id`, `received_at`; `UNIQUE (profile_id, external_event_id)`; DEFINER-only (like `feedback_ingest_idempotency`).
- DEFINERs: `mailing_claim_campaigns_for_fanout(p_limit)`; `mailing_fanout_batch(p_campaign_id, p_batch, p_message_domain)` (active subscribers, tag filter via `EXISTS subscriber_tag`, `NOT EXISTS mailing_suppression` **and** `NOT EXISTS email_suppression`, keyset on `id > fanout_cursor`, ON CONFLICT DO NOTHING, sets `fanout_done` + `recipient_count`); `mailing_claim_deliveries(p_limit, p_lease)` (queued & due, or `sending` past lease → `sending`, `attempts+1`, joined to campaign/template/subscriber/profile, fenced); `mailing_release_delivery(p_id, p_not_before)` (rate-limit deferral, `attempts-1`); `mailing_complete_delivery(p_id, p_provider_message_id)`; `mailing_fail_delivery(p_id, p_error, p_status, p_not_before)`; `mailing_cancel_campaign(p_campaign_id)`; `mailing_rollup_campaigns()`; `mailing_webhook_context(p_profile_id)`; `mailing_record_webhook(p_profile_id, p_provider, p_event_id) → bool`; `mailing_apply_provider_event(p_profile_id, p_provider_message_id, p_recipient, p_kind, p_occurred_at, p_payload)` (monotonic status, tracking event, suppression on permanent bounce/complaint, subscriber status, campaign counter, `activity_entry` when `contact_id` set — `source_type='mailing_delivery'`, `source_id=delivery_id`, `kind='mailing.<kind>'`; the 0062 dedup index absorbs replays); `mailing_mark_bounced(p_message_id) → bool` (relay bounce linkage, tenant-scoped); `mailing_enqueue_delivery(...)` (014 port — inserts a queued `automation`-sourced row, returns existing id on conflict); `mailing_delivery_engagement(p_delivery_id)`; `mailing_profile_context(p_profile_id)`.

sqlc (`db/query/mailing.sql`, header comment like `feedback.sql`): list/subscriber/tag/key/suppression/template/profile/campaign CRUD, `InsertSubscribersBatch` (unnest, ON CONFLICT DO NOTHING, `:many`), `ListSubscribersAfter` keyset + `SearchSubscribers` (`email ILIKE`, status, tag), `UpdateSubscriber` (COALESCE/NULLIF), `ListCampaignDeliveries`, `CampaignLinkStats` (GROUP BY url), `CountListSubscribersByStatus`, `GetContactsByIDs`. Every id-taking query carries `tenant_root_id = $n`.

### 5.3 Tokens (`internal/mailing/token`)

Keys: HKDF-SHA256 from `MANYFORGE_MAILING_MASTER_KEY` with info `mf-mailing/{unsub|click|open}`; the same master key drives the profile sealer (`crypto.NewSealer`).

- **Confirm**: 32 random bytes in the URL; DB stores `sha256` only, 48 h expiry, single-use (hash cleared).
- **Unsubscribe / click / open**: stateless `base64url(v1 || payload || HMAC-SHA256(purpose, v1||payload))`; payloads: unsub = `subscriber_id ‖ campaign_id` (zero uuid for list-level), open = `delivery_id`, click = `delivery_id ‖ url` (URL inside the MAC ⇒ `/m/c` is not an open redirector). Verify with `hmac.Equal` before any DB access.
- Oracle policy: bad MAC and valid-MAC-unknown-row are byte-identical (`/m/u` GET → same page; POST → 200 empty; `/m/o` → 200 gif always; `/m/c` bad MAC → 404, valid → 302 regardless of row). Tokens never logged.
- Headers on every bulk message: `List-Unsubscribe: <https://base/m/u/{tok}>`, `List-Unsubscribe-Post: List-Unsubscribe=One-Click`, `List-Id`, `Precedence: bulk`, `X-MF-Delivery`.

### 5.4 Rendering (`internal/mailing/render`)

Add `github.com/yuin/goldmark` (direct; GFM tables/autolinks; **`html.WithUnsafe()` not set** → raw HTML dropped, which is the sanitizer decision). Pipeline per content (cached in worker by campaign id / `(template_id, updated_at)`): markdown → body HTML → `layout.html` (`html/template`, embedded; from_name header, body slot, footer with `postal_address` when present + `{{unsubscribe_url}}`). Per recipient: variable substitution on the rendered output with `html.EscapeString` (never into Markdown source — `first_name` would become a link-injection vector); vars `first_name last_name email unsubscribe_url list_name`, unknown → empty. Then a `golang.org/x/net/html` tree pass: `http(s)` `<a href>` → click token URL when `track_clicks` (skip `mailto:`, `#`, unsubscribe URL); other schemes stripped; append 1×1 `<img src="/m/o/{tok}">` when `track_opens`. Plain text via `github.com/inbucket/html2text` (already indirect). Preview endpoint renders with sample vars, tracking off. Golden tests in `render/testdata`.

### 5.5 Sending profile + providers (`internal/mailing/provider`)

Resolution at send time: worker calls `mailing_profile_context(p_profile_id)` and builds a `Deliverer`; `cache.go` keys on `(profile_id, updated_at)` with 5-min TTL so rotation invalidates immediately.

- **relay.go** — wraps the process-wide `*notify.SMTPSender`; sets `m.DKIM` from `mailing_relay_identity` + DKIM sealer, `EnvelopeFrom` = from. Bounces come via existing `/inbound/bounce`: extend `inbox.DBBounceSuppressor.SuppressBounce` to call `mailing_mark_bounced(message_id)` first; when it matches, suppress tenant-scoped only, else fall through to global (decision).
- **resend.go** — hand-rolled HTTP (no SDK): `POST https://api.resend.com/emails` `{from,to,reply_to,subject,html,text,headers{List-Unsubscribe…,Message-ID},tags[{mf_delivery}]}`, `Authorization: Bearer`, `Idempotency-Key: {delivery_id}`; `{id}` → `provider_message_id`. Verify: `GET /domains` must list the from-domain `verified`. Client via `netsafe.NewClient(30s)` (uniform policy).
- **ses.go** — add `github.com/aws/aws-sdk-go-v2/service/sesv2`; promote core/config/credentials to direct. `SendEmail` with `Content.Raw = notify.BuildMIME(m)` so our headers survive; `ConfigurationSetName` from profile; no `ListManagementOptions`. Correlate via returned `MessageId` (SES rewrites Message-ID). Verify: `GetEmailIdentity(from-domain).VerifiedForSendingStatus` + `GetAccount().SendingEnabled`. Static creds from sealed JSON; region from profile (never user-supplied endpoint). Tests via custom `EndpointResolver` → httptest.
- `Classify(err)`: `notify.ErrSuppressed` → `suppressed` (terminal); 400/401/403/404/422, SES `MessageRejected`/`AccountSuspended`/invalid address → `failed` (terminal); 429/5xx/timeout/net → retry `not_before = now + min(30s·2^attempts, 30m)`; `attempts ≥ 5` → `failed`.

### 5.6 Send pipeline (`sendworker.go`, started in `main.go` next to `outboundDispatcher`, every 2 s)

1. **Fan-out**: claim due campaigns (`scheduled` & `scheduled_at <= now` → `sending`); loop `mailing_fanout_batch` in 1000-row short txs until `fanout_done` (resumable via `fanout_cursor`).
2. **Deliveries**: tx#1 `mailing_claim_deliveries(batch=100, lease=2m)`; per row `mailingLimiter.Allow(businessID)` (`ratelimit.NewTokenBucket(MailingRateRPS, Burst)`) → deny ⇒ `mailing_release_delivery(now+1s)`; else render (cached) + `Deliverer.Send` **with no tx open**; tx#2 complete/fail. `sending`+lease is set *before* the provider call; crash-after-accept re-sends after lease expiry — documented at-least-once (decision).
3. **Rollup**: `mailing_rollup_campaigns()` each tick (`sending` + `fanout_done` + no queued/sending → `sent`, counters from aggregates).

Campaign transitions (service, under principal): `draft → scheduled` via `POST /send {scheduled_at?}` (requires verified profile, active list, subject/body; warns — not blocks — when `postal_address` blank); `scheduled|sending → cancelled` (`mailing_cancel_campaign` marks queued deliveries cancelled; in-flight finish). Test-send: `POST /campaigns/{cid}/test-send {to: [≤5]}` synchronous through the profile `Deliverer`, tracking off, `[TEST]` subject prefix, no `mailing_delivery` rows, charged to the existing `outboundLimiter`.

### 5.7 Public endpoints + tracking

Ingress group (`ingestLimit`, per-key bucket like telemetry, byte-identical responses for unknown/revoked keys, `MaxBytesReader` 64 KiB):
- `POST /api/v1/mailing/public/{key}/subscribe` — JSON (`202` uniform for new/duplicate/pending) **or** HTML form post (`303` → `/m/s/{key}?state=check-inbox`); honeypot field silently accepted; CORS open like `/a/e`. Records `consent_ip`/`consent_user_agent`; `double_opt_in` ⇒ `pending` + confirmation email, sent synchronously through the business's profile `Deliverer` with tracking off (one transactional message; no `mailing_delivery` row). If the profile is unverified, the endpoint still returns the uniform 202 and logs — the list-detail UI already warns when the profile isn't verified.
- `POST /api/v1/mailing/s2s/{key}/subscribers`, `DELETE …/subscribers/{email}`, `POST …/events` (014) — `X-Mailing-Signature` + `X-Mailing-Timestamp` HMAC over `ts.method.path.body` with the `mls_` secret (feedback verified-tier pattern), ±5 min skew; `consent_source='api'`, `consent_attested_by` = key id.
- Root (`track.go`, `ingestLimit` inside handler like analytics): `GET /m/confirm/{token}` (page + button) / `POST` (confirms); `GET /m/u/{token}` (page + button) / `POST` (unsubscribes; accepts `List-Unsubscribe=One-Click` form body); `GET /m/c/{token}` (302); `GET /m/o/{token}` (gif). Angular hosted form at `/m/s/:key`; the embed snippet is a plain `<form method="post" action="…/mailing/public/{key}/subscribe">` (no server-rendered form page — dropped as duplicate).

### 5.8 Webhooks (ingress group)

- `POST /api/v1/inbound/mailing/{profileID}/ses` — 256 KiB cap; `mailing_webhook_context` (unknown/disabled → 401, same as bad signature); `snsverify.Verify`: `SigningCertURL` https + host `^sns\.[a-z0-9-]+\.amazonaws\.com(\.cn)?$`, cert fetched via netsafe + cached, canonical string per SNS spec for `Notification`/`SubscriptionConfirmation`, SignatureVersion 1 (SHA1) / 2 (SHA256) `rsa.VerifyPKCS1v15`; **`TopicArn` must equal the profile's `sns_topic_arn`** (AWS signature proves AWS, not the tenant). `SubscriptionConfirmation` → GET `SubscribeURL` (same host rule). `Notification` → `mailing_record_webhook` (dup → 200) → parse SES event (`Bounce` Permanent → bounce, Transient → ignore; `Complaint`; `Delivery`) → `mailing_apply_provider_event` keyed on `mail.messageId`. Uniform 200 after auth.
- `POST /api/v1/inbound/mailing/{profileID}/resend` — svix scheme: `svix-timestamp` ±5 min, signed `id.ts.body`, key = base64(`whsec_` suffix), `hmac.Equal` against each `v1,` entry; idempotency on `svix-id`; events `email.delivered|bounced|complained|delivery_delayed` keyed on `data.email_id` (Resend opens/clicks ignored; we track our own).

### 5.9 Outbox topics 013 emits (consumed by 014)

Constants in `internal/platform/events/bus.go`; enqueued in the mutating tx via `events.Enqueue`:
- `mailing.subscriber.activated` `{business_id, tenant_root_id, subscriber_id, list_id, email}` — exactly once per `pending→active` / direct-active add / re-activation.
- `mailing.subscriber.tag_added` `{…, subscriber_id, list_id, tag}` — only on a genuinely new tag.
- `mailing.subscriber.status_changed` `{…, subscriber_id, list_id, old_status, new_status}`.

### 5.10 API surface (`specs/013-mailing-lists/contracts/openapi.yaml`, `{id}` = business)

`mailing.read`: `GET /businesses/{id}/mailing/lists`, `/lists/{lid}`, `/lists/{lid}/subscribers` (`q,status,tag,cursor,limit`), `/lists/{lid}/subscribers/{sid}`, `/lists/{lid}/subscribers/export` (streamed CSV), `/lists/{lid}/keys`, `/sending-profile`, `/templates`, `/templates/{tid}`, `/campaigns`, `/campaigns/{cid}`, `/campaigns/{cid}/stats`, `/campaigns/{cid}/deliveries` (`status,cursor`), `/suppressions`.
`mailing.write`: `POST /lists`, `PATCH /lists/{lid}`, `DELETE /lists/{lid}` (archive); `POST /lists/{lid}/subscribers` (manual), `POST /lists/{lid}/subscribers/from-contacts {contact_ids[]}`, `POST /lists/{lid}/subscribers/import` (multipart `file` + `consent_attested=true` + `skip_confirmation`; 5 MiB / 50k rows; sniff first 512 B text; `encoding/csv` streaming; header must contain `email`; batches of 1000; sync; `{imported, skipped, errors[≤100]}`); `PATCH|DELETE …/subscribers/{sid}`; `POST /lists/{lid}/keys`, `DELETE /lists/{lid}/keys/{kid}`; `PUT|DELETE /sending-profile`, `POST /sending-profile/verify`; `POST|PATCH|DELETE /templates[/{tid}]`, `POST /templates/preview`; `POST|DELETE /suppressions[/{sup_id}]`; `POST /campaigns`, `PATCH /campaigns/{cid}` (draft only), `DELETE /campaigns/{cid}` (draft/cancelled), `POST /campaigns/preview`.
`mailing.send`: `POST /campaigns/{cid}/test-send`, `POST /campaigns/{cid}/send`, `POST /campaigns/{cid}/cancel`, `POST /sending-profile/test-send`.
Public/S2S/root/webhooks: as §5.7–5.8.

### 5.11 Config / Helm / runbook

`MANYFORGE_MAILING_MASTER_KEY` (envKey32; unset ⇒ mailing disabled, warn — nil-guard all wiring); `MANYFORGE_MAILING_RATE_RPS=10` / `_BURST=50` per business; `_SEND_BATCH=100`; `_SEND_EVERY=2s`; `_LEASE=2m`; `_MESSAGE_DOMAIN` (default `InboundSystemDomain`); `MANYFORGE_PUBLIC_BASE_URL` required for links. Helm: `secrets.masterKeys.mailingKey` env (`optional: true`, after feedbackKey in `charts/.../deployment.yaml`), rate/batch keys in configmap + values. Prod enablement follows the memory recipe (patch `manyforge-masterkeys` secret + chart env). `docs/runbooks/mailing-providers.md`: SES — configuration set with SNS event destination (Bounce, Complaint, Delivery), HTTPS subscription to the profile webhook URL, raw delivery off, IAM `ses:SendEmail ses:SendRawEmail ses:GetEmailIdentity ses:GetAccount`, paste topic ARN; Resend — webhook at profile URL, paste `whsec_`; relay — DKIM via existing flow + SPF include.

## 6. Spec 014 — automations (backend)

### 6.1 Ports from 013 (`internal/automations/ports.go`; implemented in `internal/mailing/ports014.go`, wired in `main.go`; all take `tx pgx.Tx`, principal-less, DEFINER-backed)

- `SubscriberReader`: `Snapshot(ctx, tx, subscriberID) (Snapshot{ID, BusinessID, TenantRootID, ListID, Email, Status, Tags}, error)`; `ActiveOnList(ctx, tx, businessID, email, listID) (bool, error)`; `ResolveForList(ctx, tx, businessID, listID, email) (uuid, error)`.
- `MessageSender`: `Enqueue(ctx, tx, MessageSpec{BusinessID, TenantRootID, SubscriberID, TemplateID, TrackOpens, TrackClicks, SourceKind:"automation", SourceID: stepID}) (deliveryID, error)` — **enqueue-only, transactional, idempotent on `(source_kind, source_id, subscriber_id)`** (→ `mailing_enqueue_delivery`). Provider send happens in 013's worker, so 014 has no crash-mid-node duplicate window.
- `EngagementReader`: `Engagement(ctx, tx, deliveryID) (Engagement{Opened bool, ClickedURLs []string}, error)`.
- `Tagger`: `AddTag/RemoveTag(ctx, tx, subscriberID, tag) error` (idempotent; emits `tag_added` topic on new).
- `TemplateReader.Exists`, `ListReader.Exists` (principal-bound; validation only).

### 6.2 Data model (0128–0129; numbers are placeholders after 013's 0124–0127)

Graph is stored as **one `graph jsonb` column per immutable `automation_version`** (not normalized rows): a version is a snapshot, the canvas PUTs the whole graph, validation is whole-graph, and stats/lookups are served by `node_id text` columns on enrollment/step rows. Trigger is denormalized onto the version at activate (`trigger_kind`, `trigger_ref`) for indexed fan-in.

- `automation` — `name`, `description`, `status('draft','active','paused','archived')`, `allow_reenroll bool`, `active_version_id NULL`, `draft_version_id NULL`, `created_by_principal_id`.
- `automation_version` — `automation_id`, `number`, `status('draft','active','superseded')`, `graph jsonb`, `trigger_kind NULL`, `trigger_ref NULL`, `activated_at`; `UNIQUE (automation_id, number)`; partial index `(business_id, trigger_kind, trigger_ref) WHERE status='active'`.
- `automation_enrollment` — `automation_id`, `version_id`, `subscriber_id` (FK `(subscriber_id, tenant_root_id)→list_subscriber`), `status('active','completed','exited','errored')`, `current_node_id text`, `wake_at`, `lease_expires_at`, `claim_generation` (incremented on every lease claim and matched by step/failure writes), `node_attempts`, `last_error`, `exit_reason`, `source_event_id NULL`, `enrolled_at`, `finished_at`. Unique partials `(automation_id, subscriber_id) WHERE status='active'` and `(automation_id, source_event_id) WHERE source_event_id IS NOT NULL`; partial `(wake_at) WHERE status='active'`; `(version_id, current_node_id) WHERE status='active'`; keyset `(business_id, tenant_root_id, enrolled_at desc, id desc)`.
- `automation_enrollment_step` — `enrollment_id`, `version_id`, `node_id`, `node_kind`, `attempt`, `entered_at`, `completed_at`, `outcome('entered','waiting','advanced','sent','branch_yes','branch_no','exited','error')`, `delivery_id NULL`, `detail jsonb`; **`UNIQUE (enrollment_id, node_id)`** (DAG + version pinning ⇒ a node is entered at most once ⇒ idempotency key); index `(version_id, node_id)`.
- `automation_event` — `name`, `email citext`, `subscriber_id NULL`, `occurred_at`, `properties jsonb`, `idempotency_key NULL`; unique partial `(business_id, idempotency_key)`; index `(business_id, email, name, occurred_at desc)`.
- DEFINERs: `automation_claim_due(p_now, p_limit, p_lease)` (active enrollments, `wake_at <= now`, lease free/expired, **automation.status='active'** (paused = frozen), fenced, increments `claim_generation`, `FOR UPDATE SKIP LOCKED`, returns rows + version graph); `automation_record_step(..., p_claim_generation, ...)` (generation-fenced upsert step, update enrollment current node / wake_at / status, clear lease, reset attempts, `finished_at` on terminal); `automation_fail_step(p_enrollment_id, p_claim_generation, p_error, p_terminal, p_retry_at)`; `automation_enroll_for_trigger(p_business_id, p_tenant_root_id, p_trigger_kind, p_trigger_ref, p_subscriber_id, p_source_event_id, p_now) → int` (re-asserts subscriber scope and the trigger's configured list, raises on violation; one enrollment per matching active version, ON CONFLICT DO NOTHING on both partial uniques); `automation_exit_for_subscriber(p_subscriber_id, p_tenant_root_id, p_reason)`; `automation_event_exists(p_business_id, p_email, p_name, p_since, p_within)`; `automation_step_delivery(p_enrollment_id, p_node_id) → uuid`.
- Permissions: reuse `mailing.read` (reads), `mailing.write` (CRUD, draft, enrollments, events), `mailing.send` (activate/resume).

### 6.3 Graph JSON + validation

```json
{"nodes":[{"id":"n_welcome","kind":"send_email","name":"Welcome","config":{"template_id":"…","track_opens":true,"track_clicks":true}}],
 "edges":[{"id":"e1","from":"n_trigger","to":"n_welcome","branch":null}]}
```
Node `id` `^[a-z0-9_-]{1,64}$`, unique; no positions. `config` per kind:
- `trigger`: `{"type":"list_joined","list_id"}` | `{"type":"tag_added","tag","list_id"}` | `{"type":"event","name","list_id"}` (`list_id` = which list's subscriber row is enrolled).
- `send_email`: `{"template_id","track_opens","track_clicks"}`.
- `wait`: `{"mode":"duration","seconds"}` (60…31 536 000) | `{"mode":"until","weekday":1-7|null,"time":"HH:MM","timezone":IANA}`.
- `condition`: `{"predicate": {"type":"opened_email","node_id"} | {"type":"clicked_link","node_id","url"|null} | {"type":"has_tag","tag"} | {"type":"on_list","list_id"} | {"type":"event_received","name","within_seconds"|null}}` (single predicate in v1).
- `add_tag`/`remove_tag`: `{"tag"}`; `exit`: `{}`.
- Edge `branch`: `"yes"|"no"` required on edges leaving a `condition`, `null` otherwise.

`Validate(graph, refs) []Issue{code,node_id,edge_id,message}` (stable codes; the canvas mirrors them): exactly one `trigger` with in-degree 0; all nodes reachable; acyclic (Kahn); non-condition ≤ 1 out-edge; condition exactly one `yes` + one `no`; edges reference existing nodes, no self-loops; ≤ 200 nodes; per-kind config shape; `opened_email/clicked_link.node_id` must be a `send_email` **ancestor** of the condition; template/list existence within the caller's business.

### 6.4 Execution semantics (`engine.go`, pure `Advance(ctx, tx, enrollment, graph, now) (Outcome, error)` over `Deps{Subscribers, Sender, Engagement, Tagger, Steps}`)

Loop ≤ `MaxNodesPerTick=25`: unknown node → `errored` (`graph_node_missing`). Before `send_email`/`add_tag`: `Snapshot`; status ≠ active → `exited` (reason = status). Per kind: `trigger` → advance; `send_email` → `Sender.Enqueue(SourceID=stepID)` → record `sent` + `delivery_id`; `wait` → first visit records `waiting`, sets `wake_at` (duration or `nextOccurrence(weekday,time,tz)`), returns parked; woken visit completes + advances; `condition` → evaluate → `branch_yes|branch_no` → follow that edge; tags → `Tagger`; `exit`/leaf → `completed`. Each node's result is written via `automation_record_step` in the **same tx** as the whole tick; since `Enqueue` is transactional a crash rolls back steps and deliveries together, the lease expires and the tick reruns from the persisted `current_node_id`; `(enrollment_id,node_id)` unique is the second guard. Predicates: `opened_email/clicked_link` → `automation_step_delivery` → nil ⇒ false, else `Engagement` (url null = any click, else exact match); `has_tag` from snapshot (case-insensitive); `on_list` → `ActiveOnList`; `event_received` → `automation_event_exists(since=enrolled_at)`. Errors: port/DB error → `automation_fail_step(terminal=false, retry_at=now+30s·2^n cap 1h)`; `MaxNodeAttempts=5` → `errored` with `last_error` surfaced; validation-class runtime errors (missing template) terminal immediately. `unsubscribe_from_list` node deferred to v2.

### 6.5 Triggers, exits, events

`TriggerSubscriber` (registered in `main.go` next to L616-678) handles 013's topics: `activated` → `("list_joined", list_id)`, `tag_added` → `("tag_added", tag)`, `automation.event.received` → `("event", name)`; each calls `automation_enroll_for_trigger(source_event_id = e.ID)` (idempotent under at-least-once). `status_changed` → `automation_exit_for_subscriber` (global exit; idempotent UPDATE of active rows). Custom events: `POST /businesses/{id}/mailing/events {name, email|subscriber_id, occurred_at?, idempotency_key?, properties?}` (JWT + `mailing.write`) and S2S `POST /mailing/s2s/{key}/events` (013's `mls_` HMAC); service inserts `automation_event` (dup key → 200 existing, no re-emit) and enqueues `automation.event.received` in the same tx; handler resolves the list subscriber per matching version (`ResolveForList`; not active on that list ⇒ skip).

### 6.6 Worker (`stepper.go`)

`Stepper{DB, Deps, Every: 5s, Batch: 50, Lease: 2m, MaxNodesPerTick: 25, MaxNodeAttempts: 5, Now}`: tx#1 `automation_claim_due`; per enrollment tx#2 running `Advance` (DB-only ⇒ one tx is safe); on tx failure log and let the lease expire; drain greedily while a batch is full (as `outbox.Run`). Safe on every replica (SKIP LOCKED + lease). Lifecycle: **pause** = excluded from claim (freeze; on resume elapsed waits fire immediately — documented); **archive** = exit all active (`exit_reason='archived'`) in the principal tx; **activate** = validate draft → version `active`, previous → `superseded`, set trigger cols, automation `active`; in-flight enrollments keep their `version_id`. Editing an active automation: `POST …/versions` clones active → new draft.

### 6.7 API surface (`specs/014-automations/contracts/openapi.yaml`)

Read (`mailing.read`): `GET /businesses/{id}/mailing/automations`, `…/{aid}`, `…/{aid}/versions`, `…/{aid}/versions/{vid}` (incl. graph), `…/{aid}/enrollments?status=&node_id=&cursor=`, `…/enrollments/{eid}` (with steps timeline), `…/{aid}/stats?version_id=`.
Write (`mailing.write`): `POST …/automations`, `PATCH …/{aid}` (name, description, allow_reenroll), `POST …/{aid}/versions` (clone active → draft), `PUT …/{aid}/versions/{vid}/graph` (draft only), `POST …/versions/{vid}/validate` → `{valid, issues[]}`, `POST …/{aid}/pause`, `POST …/{aid}/archive`, `POST …/{aid}/enrollments {subscriber_id}` (409 if active & not reenrollable), `POST …/enrollments/{eid}/exit`, `POST /businesses/{id}/mailing/events`.
Send (`mailing.send`): `POST …/{aid}/versions/{vid}/activate` (422 `AUTOMATION_INVALID` + issues), `POST …/{aid}/resume`.

## 7. Frontend (Angular 21, `web/`)

Conventions: single-file standalone page components with inline templates under `web/src/app/pages/<area>/`, signals, `inject()`, `data-testid` on every interactive element, hand-written services in `web/src/app/core/`, `--mf-*` tokens only (`npm run check:tokens`), `.mf-*` utilities from `web/src/styles.css`, business `<select>` pattern from `pages/crm/contacts-list.ts`.

### 7.1 Shared UI, routes, nav

Promote to `web/src/app/ui/`: `stat-tiles/stat-tiles.ts` (lifts `.mf-stats` from `pages/analytics/dashboard.ts`), `tag-chip-input/tag-chip-input.ts` (lifts chips from `pages/support/thread-view.ts`), `markdown-preview/markdown-preview.ts` (sandboxed iframe, §7.3); `ui/status.ts` tone mappers for campaign/subscriber/profile/automation/enrollment statuses; `core/unsaved-changes.guard.ts` (`CanDeactivateFn`, first in codebase).

Routes (`web/src/app/app.routes.ts`, insert before `credentials/connector`; literal before param): `mailing → mailing/lists`; `mailing/lists | mailing/campaigns | mailing/templates | mailing/sending | mailing/suppression | mailing/automations` (guarded); `mailing/:businessId/lists/:listId`; `mailing/:businessId/campaigns/:campaignId/stats` (before the editor route); `mailing/:businessId/campaigns/:campaignId`; `mailing/:businessId/templates/:templateId`; `mailing/:businessId/automations/:automationId`; public `m/s/:key | m/confirm/:token | m/u/:token` (no guard). `App.isPortalRoute` → `startsWith('/p/') || startsWith('/m/')`; `authInterceptor.skipRefresh` adds `/api/v1/mailing/public/`. Nav (`ui/nav.ts`): `Mailing → /mailing/lists`, `Campaigns → /mailing/campaigns`, `Automations → /mailing/automations` after Feedback; update `nav.spec.ts`'s exact array.

### 7.2 013 pages (`pages/mailing/`, each with colocated `.spec.ts`)

`lists-list.ts`; `list-detail.ts` (public key + hosted URL + embed `<form>` snippet w/ copy, subscribers table w/ status pills, tags, `q/status/tag` filters, cursor "Load more"; hosts `subscriber-import.ts` (file input, consent checkbox gating submit, `FormData`) and `contacts-picker.ts` (paginates `CrmService.listContacts`, checkboxes → `from-contacts`)); `sending-profile.ts` (mode radio relay|resend|ses; relay: `<select>` of verified `EmailDomain`s via existing `ticket.service.ts listEmailDomains()`; BYO: provider select + secret inputs shown only until saved — server returns `has_credentials`, never the secret, mirroring board-detail's write-once panel; Verify / Send test; status pill; postal-address warning banner); `templates-list.ts` + `template-editor.ts` (reuses the editor pane component); `campaigns-list.ts`; `campaign-editor.ts` (§7.3); `campaign-stats.ts` (`mf-stat-tiles` + per-link table + deliveries table); `suppression-list.ts`; `public/subscribe.ts`, `public/confirm.ts`, `public/unsubscribe.ts` (§7.5). Services: `core/mailing.service.ts` (DTOs mirror §5.10; subscriber status values `pending|active|unsubscribed|bounced|complained`), `core/public-mailing.service.ts`.

### 7.3 Campaign/template editor + preview

Two-pane `mf-card` grid (stacks < 960 px): form (subject, preheader, from defaults from profile, list `<select>`, tag filter chips, variables palette inserting at `selectionStart/End`, `mf-textarea` body, tracking toggles) | preview with HTML/Text toggle. Preview: plain `setTimeout` 400 ms debounce + `previewSeq` stale-guard (codebase style, no rxjs `debounceTime` precedent) → `POST …/campaigns/preview` → `{html, text}`. Rendering: `<iframe sandbox="" title="Email preview" referrerpolicy="no-referrer">` with `srcdoc` set **imperatively** via `viewChild` (Angular's `[srcdoc]` binding sanitizes away `<style>`; imperative set avoids introducing `DomSanitizer`); empty `sandbox` blocks scripts/forms/navigation/same-origin; own document root ⇒ no global CSS bleed, dark-mode independent (`color-scheme: light`). Fixed 70 vh, scroll inside. Button state machine by status: draft → Save/Send test/Schedule/Send now; scheduled → Cancel (read-only fields); sending → Cancel disabled + spinner; sent/cancelled/failed → read-only + View stats. Save disabled until the sending profile is `verified` (banner → `/mailing/sending`). Schedule: `datetime-local` + browser-timezone hint → ISO.

### 7.4 Automation canvas (`pages/mailing/automations/`)

Files: `automations-list.ts`; `automation-editor.ts` (shell: header, lifecycle buttons, version banner, tabs Canvas | Enrollments, owns graph store + save/activate); `canvas/layout.ts` (pure `layoutGraph`); `canvas/graph-ops.ts` (pure `insertNode`, `deleteNode`, `retargetExit`, `validateGraph`, `defaultConfig`, `summarizeNode`); `canvas/automation-canvas.ts` (viewport, pan/zoom, nodes, edges, `+` buttons, keyboard); `canvas/node-panel.ts` (`@switch (kind)` forms); `enrollments-tab.ts`; `core/automations.service.ts`.

State (editor, passed down via `@Input`): `graph`, `savedJson`, `selectedId`, `serverErrors`, `statsOn`, `stats` signals; computed `clientErrors = validateGraph(graph())`, `layout = layoutGraph(graph(), NODE_SIZE)`, `dirty`, `readOnly = version.status !== 'draft'`.

Layout algorithm (deterministic, O(V+E)):
```
sizes: trigger 220x64, condition 220x72, send_email 220x80, others 200x56; GAP_X=40, GAP_Y=64
1 adjacency: out[u] sorted (yes, no, null, edge.id); in[v] likewise
2 rank = longest path from trigger (Kahn); cycle → DFS-mark back edges, ignore, rerank;
  unreachable → rank maxRank+1, placed far right (validation flags them)
3 tree-ify the DAG: treeParent[v] = in-neighbour with greatest rank (tie: smaller edge.id);
  children[u] ordered yes-left / no-right
4 post-order width[v] = max(w(v), Σ width[children] + GAP_X*(n-1))
5 pre-order x: children placed left→right; parent centred over first..last child
6 per rank sort by x, push right on overlap (catches merge nodes with far-away cross parents)
7 y = Σ (rowH[r'] + GAP_Y) for r' < rank
8 edges: cubic from bottom-centre(src) to top-centre(dst); '+' at path midpoint; branch label at t=0.25
9 normalise to padding → {nodes[{id,x,y,w,h,rank}], edges[{id,path,plus,label}], width, height}
```
No crossing-minimisation passes (yes-left/no-right already avoids overlap for automation-shaped DAGs; documented limitation).

Rendering: HTML node cards absolutely positioned inside `<div class="world" [style.transform]="translate(px,py) scale(z)">` + sibling `<svg>` of paths/labels; `+` affordances are real `<button data-testid="edge-plus" data-edge-id>` in the world div. Nodes rendered in (rank, x) order so DOM order = visual order. Card: kind icon, name, `summarizeNode()` line, `data-testid="canvas-node" data-node-id data-kind data-invalid aria-selected`. (All-SVG/`foreignObject` rejected: inconsistent text wrapping/focus, worse a11y and e2e.)

Interaction: pan via `pointerdown` on background + `setPointerCapture`; wheel pans, `ctrl/meta+wheel` zooms around cursor (0.25–2), `+/−/Fit` buttons. Insert on edge A→B(branch b): picker popover → `insertNode`: N with `crypto.randomUUID()`, A→N(b), N→B(null); inserting a `condition` sets N→B `yes` and adds a fresh `exit` E with N→E `no` (a condition always owns exactly one yes + one no from birth). Merges: an `exit` node's panel offers "Instead, continue to…" (non-ancestor nodes only) → `retargetExit`; the only way merges are created. Delete: trigger undeletable; plain node splices A→B preserving branch; merge node rewires all in-edges to its successor; condition: yes-branch re-attached to parent, no-only-reachable subtree removed after confirm (count shown); sole-terminator exit undeletable (retarget instead). Selection + side panel edits `graph.update()` immediately; Save = `PUT graph`. Keyboard: canvas `role="application"`, selected node `tabindex=0`, arrows navigate (down = first out, up = tree parent, left/right = rank siblings), Enter opens panel, Delete deletes, Escape closes. Undo: YAGNI (Discard resets to `savedJson`); unsaved-changes guard + `beforeunload`.

Validation mirrored client-side (same rules as §6.3 minus existence checks); errors → red ring + `data-invalid` + title on node, count in header, list in panel; Activate disabled while client errors exist; server 422 issues merged by `node_id`. Lifecycle: draft → Activate; active → Pause / Edit (`POST versions` → navigate to draft; banner "editing v3, v2 stays live"); paused → Resume / Edit; any → Archive (confirm). Non-draft versions read-only. Stats overlay (non-draft): per-node `entered/completed` pills, send_email adds sent/opened/clicked.

### 7.5 Public pages

Bare layout via the extended `portalRoute()` branch, `mf-theme-toggle`, "Powered by ManyForge" footer as in `pages/feedback/portal.ts`. `/m/s/:key`: list name + email/first-name inputs → POST → always "Check your inbox to confirm" (only network errors show retry). `/m/confirm/:token` and `/m/u/:token`: GET renders a button; POST → identical done card regardless of response. The three done states share one template string; no token or email echoed.

## 8. PR slicing (one branch off `master` at a time; each merges before the next starts)

Each backend slice adds its paths to the spec's `openapi.yaml` so the drift test stays green both ways. Each slice = bd issue under an epic per spec.

| # | Slice | Gate |
|---|---|---|
| 0 | **Bootstrap**: commit this design as `docs/superpowers/specs/2026-08-26-mailing-lists-automations-design.md`; create `specs/013-mailing-lists/{spec.md,plan.md,data-model.md,contracts/openapi.yaml}` and `specs/014-automations/…` skeletons; bd epics + issues per slice; add `mailing.*` entries to ROADMAP SL-D | review |
| 1 | **013-BE core**: 0124 + schema.sql + `mailing.sql` + `make generate`; lists/subscribers (manual, CRM, CSV import)/tags/keys/suppression/templates/profile CRUD (no verify); permissions + authz consts; wiring; merge inventory; `drift_013_test.go` | unit validation tests; integration isolation test (feedback-style); drift; pins (RLS+WITH CHECK, tenant predicate, manifest/fence, perm constants) |
| 2 | **013-FE A**: `mailing.service`, lists-list, list-detail, import, contacts-picker, tag-chip-input, templates list/editor (no preview yet), routes/nav | vitest specs; Playwright `e2e/mailing-lists.spec.ts` (with `**/api/**` fallback route) |
| 3 | **013-BE public + tokens**: 0125, token codec, public subscribe (JSON+form), confirm/unsubscribe pages (GET button/POST), S2S subscribers, `/m/*` root routes, outbox topics (§5.9) | codec unit tests (tamper/truncate/wrong purpose); oracle pins (byte-identical); double-opt-in integration flow |
| 4 | **013-FE B**: sending-profile page, `public-mailing.service`, `/m/*` Angular pages, `app.ts`/interceptor changes | vitest; `e2e/mailing-public.spec.ts` |
| 5 | **013-BE render + providers**: render package + goldens, `Deliverer`, relay/resend/ses, `notify.BuildMIME`, profile verify, profile test-send, preview endpoints, Helm values + master key, runbook | golden render tests; httptest fakes for Resend/SES (classification matrix); SES `EndpointResolver`→httptest |
| 6 | **013-BE campaigns + worker**: 0126, `mailing_delivery`, campaign CRUD/send/cancel/stats, fan-out, dispatcher, rollup, `/m/c` `/m/o` tracking, relay bounce linkage | integration: fan-out excludes suppressed/unsubscribed/tag-mismatch; lease reclaim; no double-send within lease; resumable fan-out; rate-limit deferral; tracking events |
| 7 | **013-FE C**: `markdown-preview` iframe, campaign editor/list, template preview, unsaved guard | vitest incl. `<style>`-bearing srcdoc fixture; `e2e/mailing-campaigns.spec.ts` |
| 8 | **013-BE webhooks**: SNS verify + Resend verify, event mapping, CRM activity, remaining pins | SNS test with generated RSA cert + injected fetcher; svix vectors; idempotency + TopicArn mismatch |
| 9 | **013-FE D**: stat-tiles, campaign-stats, suppression-list | vitest; e2e additions |
| 10 | **014-BE schema**: 0128–0129, schema.sql, `automations.sql`, inventory maps, pins | `make test`, `make sec-test` |
| 11 | **014-BE graph + CRUD/lifecycle API**: `graph.go` Validate, service, handler, `drift_014_test.go` | validation table tests; integration CRUD/versioning/pause/archive/cross-tenant 404 |
| 12 | **014-FE A**: `automations.service`, list, `layout.ts`, `graph-ops.ts`, canvas, node-panel, save (no activate) | `layout.spec` fixtures (linear, branch, nested, merge, cycle/orphan — relative ordering only); `graph-ops.spec`; component specs; `e2e/automations.spec.ts` (insert → edit → save, PUT body asserted) |
| 13 | **014-BE engine + stepper + 013 ports**: ports, `ports014.go` + `mailing_enqueue_delivery`, fakes, `Advance`, `Stepper`, wiring | unit (every node kind, wait park/resume, predicates, exits, retry→errored, MaxNodesPerTick); integration claim/lease/SKIP LOCKED/paused/crash-rollback/replay unique |
| 14 | **014-BE triggers/exits/events/enrollments/stats**: outbox subscribers, events API (JWT + S2S), enrollment endpoints, per-node stats; **golden scenario** (welcome → wait 1d → clicked? → yes: tag / no: reminder) with injected clock | integration + security pins (404-uniform, foreign subscriber_id 404, exit scoped to tenant, claim excludes fenced roots) |
| 15 | **014-FE B**: activate/pause/resume/archive, server-error mirroring, versions banner | vitest; e2e activate 422→highlight→200 |
| 16 | **014-FE C**: stats overlay + enrollments tab | vitest; e2e |

## 9. Test plan (summary; details per slice above)

- **Unit** (`make test`): token codec; render (escaping of `first_name` containing markup, raw HTML dropped, scheme filtering, link-rewrite skip rules, pixel toggle, text part); `provider.Classify`; Resend/SES clients vs `httptest`; `snsverify` (cert host pin, v1/v2, bad canonical string, http cert URL rejected); svix verify (skew, multi-sig, bad base64); CSV parser caps/sniff; graph `Validate`; `Advance` with fakes + fixed clock; `nextOccurrence` DST; layout/graph-ops pure functions.
- **Integration** (`//go:build integration`, `testdb.Start` testcontainers as in `internal/feedback/feedback_integration_test.go`): cross-business/tenant isolation for every table (404 not 403); DEFINERs pinned/REVOKEd; subscribe→confirm→active; unsubscribe suppresses + blocks fan-out; fan-out/claim/lease/rollup; webhook idempotency + status monotonicity; `activity_entry` dedup; enrollment claim/lease/replay; golden automation scenario; merge inventory test green.
- **Contract**: `drift_013_test.go`, `drift_014_test.go` (both directions) via `make contract-test`.
- **Security regression** (`internal/security_regression/mailing_pins_test.go`, `automations_pins_test.go`): RLS + WITH CHECK on every table; no `authorized_tenants`; `tenant_root_id` in every id-taking query; DEFINER `search_path` + REVOKE; `hmac.Equal` in token/svix/sns; no token/secret fields in `slog` calls; single response func for `/m/u`; `netsafe.NewClient` in providers; SNS host regex present; `Idempotency-Key` in resend.go; fence trigger per table; perm constants pinned.
- **Frontend**: vitest per component; Playwright specs per slice (route mocks, `data-testid`/DOM-order assertions only, `**/api/**` fallback first); `npm run check:tokens`; `ng build` production.

## 10. End-to-end verification (after slices 8 and 14)

1. Local full-app (memory recipe): compose DB :55432, `make migrate`, `.air.env` API :8081, `MANYFORGE_MAILING_MASTER_KEY` set, `LogSender`-backed relay.
2. Create business → sending profile (relay + verified `email_domain` seeded) → list (double opt-in) → hosted form subscribe → confirm via `/m/confirm` POST → subscriber `active`.
3. Campaign: Markdown body with two links → preview → test-send → send now → worker fan-out → deliveries `sent` (LogSender output shows `List-Unsubscribe`, rewritten links, pixel) → hit `/m/c` and `/m/o` → stats update → `/m/u` POST → subscriber `unsubscribed` + suppression row → second campaign's fan-out excludes them.
4. Automation: trigger list_joined → welcome (template) → wait → clicked? → yes: add_tag / no: reminder. Activate; subscribe a new address; advance the clock (integration test) or wait; verify enrollment timeline + deliveries + tag.
5. Provider webhooks: post a signed SNS bounce fixture and a svix-signed Resend `email.bounced` → delivery `bounced`, subscriber `bounced`, tenant suppression, CRM `activity_entry` when linked.
6. Real-provider smoke (manual once, documented, not a gate): Resend sandbox key on a test business; SES sandbox with an SNS HTTPS subscription against a dev tunnel.

## 11. Out of scope / follow-ups (file as bd issues)

Async blob-backed CSV import; multiple sending profiles per business; generic SMTP provider; `unsubscribe_from_list` node; compound predicates (`all/any`); per-business claim fairness caps in the stepper; free-drag canvas; block/WYSIWYG editor; A/B subject tests; retrofitting the existing transactional mailer (`token: <raw>` bodies) onto the new layout (SL-D); RSA DKIM fallback (`manyforge-3jt`); feedback status-change notifications (`manyforge-dt8j`) via the new templating.

## 12. Open risks to watch

- SES rewrites `Message-ID` ⇒ two correlation keys (ours for relay, provider id for Resend/SES) — handled in `mailing_apply_provider_event`.
- Burst: a 10k import into a welcome automation creates 10k enrollments due at once → ~17 min drain at Batch 50/5 s and a flood into the delivery queue; tune batch/greedy drain if it matters at launch.
- Relay reputation: bulk mail via the shared relay uses the relay's IP/SPF even with tenant DKIM; consider a lower `MAILING_RATE_RPS` for relay mode.
- If prod ever adds a `style-src` CSP without `'unsafe-inline'`, the `srcdoc` preview needs a navigated `src` URL with its own CSP.
