# Implementation Plan: Branching Drip Automations

**Spec:** 014

**Depends on:** Spec 013 mailing ports and delivery queue

**Design source:**
[`docs/superpowers/specs/2026-08-26-mailing-lists-automations-design.md`](../../docs/superpowers/specs/2026-08-26-mailing-lists-automations-design.md)

## Technical Context

- Backend module: `internal/automations`, with sqlc queries in
  `db/query/automations.sql` and migrations after Spec 013.
- Integration seam: transactional interfaces implemented by
  `internal/mailing/ports014.go`; automations never send to a provider directly.
- Frontend: Angular editor under `web/src/app/pages/mailing/automations/`, with
  pure layout and graph-operation modules and a hand-written API service.
- Contract: `contracts/openapi.yaml`, expanded by backend API slices and pinned
  bidirectionally by `drift_014_test.go`.

## Constitution Check

- Tenant and security controls mirror Spec 013, including dual predicates, RLS,
  locked-down definers, merge fences, and uniform 404s.
- Module boundaries are enforced with subscriber, message, engagement, tag,
  template, and list ports rather than direct access to mailing tables.
- Engine state and queued delivery are atomic in one transaction; leases and
  uniqueness constraints provide crash and replay safety.
- Every slice is developed test-first and adds the relevant unit, integration,
  contract, security, component, and browser evidence.
- The canvas is keyboard operable and deterministic; client validation mirrors
  structural server validation while the server remains authoritative.

## Architecture

An `automation` owns mutable lifecycle metadata and references an active and/or
draft version. Each `automation_version` is an immutable JSON graph snapshot once
activated. Enrollments pin a version and current node. A short database claim
transaction leases due enrollments; a second transaction runs the pure advance
engine and atomically records steps, enrollment state, tag changes, and any
mailing delivery enqueue.

Outbox consumers convert subscriber and custom events into idempotent
enrollments and exit active enrollments when the subscriber leaves an eligible
state. The browser derives all node positions from the graph, so content and
versioning remain independent of presentation.

## Delivery Slices

1. Schema: automation, version, enrollment, step, and event tables; claim and
   mutation definers; schema mirror, merge inventory, and security pins.
2. Backend graph and lifecycle: validation, CRUD, immutable version transitions,
   lifecycle/enrollment APIs, drift contract, and integration isolation tests.
3. Frontend A: service, list, deterministic layout, graph operations, canvas,
   node panel, save, unit fixtures, and save-flow Playwright test.
4. Engine and stepper: Spec 013 ports, pure `Advance`, wait/predicate behavior,
   bounded retry, lease claim, crash rollback, and replay tests.
5. Triggers and observability: outbox enrollment/exits, JWT and S2S events,
   enrollment/timeline/stat endpoints, and the golden scenario.
6. Frontend B: activate, pause, resume, archive, server-error mirroring, and
   version banner.
7. Frontend C: per-node statistics overlay and enrollments tab with manual
   enroll/exit.

Each slice branches from current `master`, merges before the next begins, and
uses the detailed acceptance gate in section 8 of the canonical design.

## Verification

The final verification combines graph and engine unit matrices, integration
tests for every lease/lifecycle/tenant boundary, bidirectional API drift,
security pins, Angular component tests, token/build checks, and Playwright. The
injected-clock golden scenario is the authoritative end-to-end engine proof.
