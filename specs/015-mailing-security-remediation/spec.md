# Feature Specification: Mailing and Automation Security Remediation

**Spec:** 015

**Status:** Approved for local implementation; private until coordinated disclosure

**Canonical evidence:** [`SCAN.md`](../../SCAN.md)

## Problem

Specs 013 and 014 shipped with 23 confirmed security defects spanning consent,
lifecycle enforcement, provider feedback, webhook processing, automation
authorization and fencing, shared-resource bounds, capability logging, and
client URL construction. Several defects share worker and SQL boundaries, so
isolated symptom patches would leave equivalent paths exploitable.

## Outcome

Mailing and automation operations enforce consent, tenant and resource
lifecycle, least privilege, immutable activation, authenticated provider
feedback, idempotent event processing, bounded work and retention, and safe
capability handling at every ingress and final side-effect boundary.

## Global Security Invariants

1. A public subscribe request never reactivates an unsubscribed or suppressed
   recipient without a fresh mailbox-confirmed flow.
2. Principal-less resolvers and workers require an active, non-deleted business;
   list-bound confirmation, enrollment, fan-out, claim, and final renewal also
   require an active list. Unsubscribe remains available after archive.
3. A non-nil durable consent or provider-feedback error produces a generic
   retryable response; malformed, unknown, replayed, and successful no-ops keep
   their uniform response.
4. Bulk sending requires outbound identity verification and durable feedback
   readiness. SES readiness is recorded only after successful subscription
   confirmation; relevant profile changes reset readiness.
5. `mailing.send` is required at handler and service boundaries for explicit
   send-producing automation operations. Active automation mail content is
   immutable until a send-authorized reactivation.
6. Automation activation atomically binds the exact validated graph version.
   Pause, archive, exit, and generation changes fence every side effect before
   it occurs.
7. Event identity includes immutable ingress scope and canonical request
   fingerprint. Every accepted S2S event has replay protection, and event time
   cannot satisfy conditions before it occurs.
8. Provider webhook authentication precedes attacker-influenced outbound work;
   cardinality and payload persistence are bounded. Authenticated early events
   remain retryable or durably pending until delivery correlation exists.
9. Tracking, version history, fan-out, rollups, and in-memory caches have
   explicit bounds, eviction, or aggregation. Deleting a sending profile
   invalidates credential-bearing clients.
10. Every live send path, including tests, applies suppression policy or records
    an explicit audited override.
11. Request and error logs never contain capability tokens, subscriber emails in
    secret-bearing paths, credentials, or complete message bodies.
12. Every client-supplied route identifier is canonicalized and encoded as one
    path segment before network use.

## Finding Requirements

### Consent and lifecycle

- **MF-MAIL-PUB-001:** preserve unsubscribe and suppression during anonymous
  subscribe; require confirmed reactivation.
- **MF-MAIL-DB-001:** add active/non-deleted business predicates to every public,
  S2S, profile, campaign, delivery, and automation system path.
- **MF-MAIL-ERRACK-001:** return generic retryable failure after authenticated
  persistence failure instead of acknowledging success.
- **MF-MAIL-LIFECYCLE-002:** archive terminalizes or pauses pending outbound work
  and active-list predicates protect confirmation and every worker sink.
- **MF-MAIL-TRACK-001:** make open/click replay storage idempotent or aggregated
  and retain raw detail only within a fixed policy.

### Provider and webhook integrity

- **MF-MAIL-WEBHOOK-001:** cap and deduplicate provider recipients and persist a
  provider envelope once.
- **MF-MAIL-WEBHOOK-002:** authenticate the signed envelope without allowing an
  untrusted certificate URL to trigger fetches outside a strict cached trust
  strategy; compare profile/topic binding before provider-event acceptance.
- **MF-MAIL-WEBHOOK-003:** do not permanently consume an event idempotency key
  before delivery correlation can succeed.
- **MF-MAIL-FEEDBACK-001:** split outbound verification from feedback readiness
  and require both at schedule, claim, and final renewal.
- **MF-MAIL-DELIVERY-001 provider variant:** bound the provider client cache and
  invalidate it on profile update/delete.

### Automation integrity and authorization

- **AUTOMATION-EVENT-SCOPE-001:** persist list/key ingress scope and compare a
  canonical fingerprint on idempotency collisions.
- **AUTOMATION-S2S-REPLAY-003:** require an idempotency key or consume a signed
  nonce for every accepted S2S event.
- **AUTOMATION-EVENT-TIME-004:** reject excessive future timestamps and enforce
  `occurred_at <= evaluation_time` in conditions.
- **AUTOMATION-FENCE-002:** fence generation and lifecycle before each mail/tag
  side effect; a lost fence returns a typed non-success outcome.
- **AUTOMATION-SEND-AUTHZ-005:** require `mailing.send` for manual enrollment and
  send-producing event injection, and freeze referenced content at activation.
- **MF-AUTO-001:** lock or compare-and-swap the version graph identity validated
  during activation.
- **MF-AUTO-002:** paginate version history and cap retained full graph versions
  without deleting versions still referenced by enrollments.

### Delivery and shared resources

- **MF-MAIL-DELIVERY-001:** replace process-lifetime content/provider maps with
  bounded caches and explicit invalidation.
- **MF-MAIL-DELIVERY-002:** apply suppression to profile and campaign test sends;
  any override is explicit, permission-checked, and audited.
- **MF-MAIL-DELIVERY-003:** production startup fails closed when transport is
  absent; development logging emits metadata only and never reports provider
  acceptance.
- **MF-MAIL-DELIVERY-004:** one worker tick performs a bounded amount of fan-out
  per campaign and globally.
- **MF-MAIL-ROLLUP-001:** roll up incrementally from changed deliveries instead
  of rewriting all terminal campaigns on every tick.

### HTTP and frontend capabilities

- **MF-MAIL-LOG-001:** log route patterns or centralized redacted paths across
  request, recovery, and error logs.
- **MF-WEB-013-001:** canonical UUID validation plus `encodeURIComponent` for all
  mailing and automation route segments, including destructive methods.

## Migration and Compatibility

- Add forward-only migrations beginning at `0132`; never modify shipped
  migrations `0124`–`0131`.
- Down migrations remove only Spec 015 objects and restore replaced function
  definitions where rollback is safe.
- Regenerate sqlc code with `make generate`; generated files are never edited.
- Existing API error shapes remain stable except authenticated persistence
  failures become generic retryable failures and unbounded lists become
  paginated.
- Existing active campaigns and automations become non-sendable until their
  business/list/profile feedback invariants are satisfied.

## Test Plan

- Invert every `MF-*` and `AUTOMATION-*` characterization from `SCAN.md` into a
  safe regression assertion after its fix.
- Add DB-backed tests for archived/deleted lifecycle predicates, reactivation,
  feedback readiness, early webhook correlation, event fingerprints, final
  generation fences, incremental rollup, and bounded fan-out.
- Add custom-role integration tests proving `mailing.write` cannot create
  send-producing work without `mailing.send`.
- Add deterministic unit tests for cache eviction/invalidation, recipient caps,
  log redaction, timestamp validation, and metadata-only fallback behavior.
- Expand Angular service tests across slash, backslash, dot-segment, query, and
  fragment inputs for every dynamic mailing/automation method.
- Run `make test`, `make sec-test`, `make int-test`, `make lint`, the full Angular
  suite, and the approved Semgrep rules before completion.

## Out of Scope

New mailing features, provider-native automation, UI redesign, unrelated
refactoring, staging/production traffic, public disclosure, and compatibility
shims that preserve a vulnerable path.
