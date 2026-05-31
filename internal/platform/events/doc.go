// Package events is the thin first cut of shared layer SL-C (spec 002): a
// transactional outbox plus an in-process event bus.
//
// Side-effects (events, outbound mail, notification fan-out) are enqueued in the
// SAME transaction as the source mutation (no fire-and-forget); an at-least-once
// worker drains pending rows with FOR UPDATE SKIP LOCKED and dispatches to
// idempotent subscribers that dedupe on the outbox id.
package events
