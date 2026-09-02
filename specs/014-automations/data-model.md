# Data Model: Branching Drip Automations

**Spec:** 014

**Canonical detail:** sections 6.1–6.6 of the
[`mailing-lists and automations design`](../../docs/superpowers/specs/2026-08-26-mailing-lists-automations-design.md).

## Migrations 0128–0129

- `automation`: business-owned definition, lifecycle status, re-enrollment
  policy, active/draft version pointers, and creator.
- `automation_version`: numbered JSON graph snapshot, draft/active/superseded
  lifecycle, and denormalized active trigger index.
- `automation_enrollment`: subscriber/version pin, current node, wake and lease
  state, monotonically increasing claim generation, retry/error state,
  source-event idempotency, and terminal outcome.
- `automation_enrollment_step`: one idempotent node observation per enrollment,
  with outcome, timing, delivery correlation, and bounded detail.
- `automation_event`: tenant custom event with subscriber/email resolution,
  occurrence time, properties, and optional business-local idempotency key.

## Graph Value Object

Each version stores:

```json
{
  "nodes": [{"id": "n_trigger", "kind": "trigger", "name": "Joined", "config": {}}],
  "edges": []
}
```

Node identifiers are unique lowercase tokens up to 64 characters. The graph has
exactly one root trigger, is acyclic and fully reachable, caps at 200 nodes, and
uses only the node and branch shapes defined in section 6.3 of the canonical
design. No x/y presentation coordinates are stored.

## Required Invariants

- An automation version number is unique within its automation; only active
  versions are indexed for trigger fan-in.
- Only one active enrollment exists per automation/subscriber; a source event
  can enroll an automation at most once.
- `(enrollment_id, node_id)` is unique because a valid DAG version enters a node
  at most once.
- Active enrollments are due only when wake time has passed and their lease is
  free or expired; each claim increments a generation that every step/failure
  write must match, so an expired worker cannot overwrite a reclaimed lease.
  Paused automations and fenced tenant roots are unclaimable.
- In-flight enrollments retain their version when a new version activates.
- Every tenant table and foreign key preserves business/root scope, uses dual
  predicates and RLS, and participates in tenant-merge inventory and fencing.
- Security-definer enrollment, claim, step, failure, exit, event, and engagement
  functions use a pinned search path and are revoked from `PUBLIC`.

The migration SQL, `db/schema.sql`, sqlc queries, and this document must remain
consistent as the implementing slices land.
