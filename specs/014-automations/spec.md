# Feature Specification: Branching Drip Automations

**Spec:** 014

**Status:** Approved; begins after Spec 013

**Canonical design:**
[`docs/superpowers/specs/2026-08-26-mailing-lists-automations-design.md`](../../docs/superpowers/specs/2026-08-26-mailing-lists-automations-design.md)

## Problem

Spec 013 can send one broadcast, but tenants also need durable, event-triggered
sequences that branch on subscriber state and engagement. Provider-native
automation would couple product behavior to one vendor and would bypass
ManyForge's tenant, audit, suppression, and delivery guarantees.

## Outcome

An administrator can model a directed acyclic automation with one trigger and
send, wait, condition, tag, and exit nodes; validate it; activate an immutable
version; inspect and control enrollments; and understand per-node outcomes in an
accessible, automatically laid-out canvas.

## User Scenarios

1. An administrator creates a list-joined automation, inserts welcome, wait,
   engagement condition, tag, reminder, and exit nodes, then saves it.
2. Validation identifies cycles, unreachable nodes, invalid branches,
   malformed configuration, and foreign or missing references on the affected
   node or edge.
3. Activating a draft creates an immutable live version while later edits occur
   in a cloned draft; existing enrollments stay pinned to their version.
4. Subscriber activation, tag addition, or a custom event enrolls the matching
   subscriber exactly once; unsubscribe or another inactive status exits active
   enrollments.
5. An administrator pauses, resumes, archives, manually enrolls or exits, and
   inspects enrollment timelines and per-node stats.

## Functional Requirements

- Store each immutable automation version as one JSON graph snapshot with no
  persisted canvas coordinates.
- Support `trigger`, `send_email`, `wait`, `condition`, `add_tag`, `remove_tag`,
  and `exit` node kinds and the locked config shapes in the canonical design.
- Validate a single reachable trigger, acyclicity, degree/branch rules, unique
  IDs, configuration, ancestor engagement references, and tenant-local list and
  template references.
- Execute due enrollments through a pure advance engine and lease-based,
  multi-replica-safe stepper with bounded nodes per tick and bounded retries.
- Enqueue email transactionally through Spec 013's idempotent `MessageSender`
  port; provider I/O remains in the Spec 013 delivery worker.
- Enroll and exit from outbox events and accept idempotent JWT and S2S custom
  events.
- Provide lifecycle, versions, validation, enrollment, timeline, and per-node
  statistics APIs using `mailing.read`, `mailing.write`, and `mailing.send`.
- Provide an auto-layout Angular canvas with edge insertion, controlled merges,
  safe deletion, pan/zoom, keyboard navigation, validation highlighting,
  lifecycle controls, statistics overlay, and enrollment views.

## Non-Functional Requirements

- The engine persists a whole tick and any mailing enqueue in one transaction;
  a crash either commits all node progress or none of it.
- `(enrollment_id, node_id)` and Spec 013 delivery uniqueness are independent
  replay guards.
- Paused automations are excluded from claims; archived automations exit active
  enrollments; claims exclude tenant-merge-fenced roots.
- All tenant isolation, typed-error, uniform-404, RLS, security-definer, audit,
  and testing requirements inherited from the constitution apply.
- Canvas layout is deterministic and O(V+E), and DOM order follows visual order.

## Success Criteria

- The golden welcome → wait → engagement branch scenario executes
  deterministically under an injected clock and produces the expected delivery,
  tag, reminder, step timeline, and terminal state.
- Crash rollback, lease reclaim, replay, pause, archive, and cross-tenant tests
  prove the documented lifecycle invariants.
- The browser test can insert, edit, validate, save, activate, and inspect a
  graph without relying on stored coordinates or mouse-only interaction.

## Out of Scope

Unsubscribe nodes, compound predicates, provider-native workflows, free-drag
layout, persisted positions, undo history beyond discard-to-saved, A/B testing,
and per-business claim fairness caps.
