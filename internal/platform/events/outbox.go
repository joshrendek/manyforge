package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// Enqueue writes an outbox row in the SAME transaction as the source mutation
// (Principle VI — no fire-and-forget). payload is JSON-encoded; the id is uuid v7
// so the drain order is stable/monotonic.
func Enqueue(ctx context.Context, tx pgx.Tx, tenantRootID uuid.UUID, topic string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", topic, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("outbox id: %w", err)
	}
	return dbgen.New(tx).EnqueueOutbox(ctx, dbgen.EnqueueOutboxParams{
		ID: id, TenantRootID: tenantRootID, Topic: topic, Payload: raw,
	})
}

// Worker drains the outbox at-least-once and dispatches to Bus subscribers.
type Worker struct {
	DB          *db.DB
	Bus         *Bus
	Logger      *slog.Logger
	BatchSize   int
	PollEvery   time.Duration
	MaxAttempts int32
}

func (w *Worker) withDefaults() {
	if w.BatchSize <= 0 {
		w.BatchSize = 50
	}
	if w.PollEvery <= 0 {
		w.PollEvery = time.Second
	}
	if w.MaxAttempts <= 0 {
		w.MaxAttempts = 10
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
}

// Run polls until ctx is cancelled, draining greedily while batches are full so
// a backlog clears without waiting a whole poll interval.
func (w *Worker) Run(ctx context.Context) {
	w.withDefaults()
	w.Logger.InfoContext(ctx, "outbox worker started")
	t := time.NewTicker(w.PollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for {
				n, err := w.drainOnce(ctx)
				if err != nil {
					w.Logger.ErrorContext(ctx, "outbox drain", "err", err)
					break
				}
				if n < w.BatchSize {
					break
				}
			}
		}
	}
}

// drainOnce claims a batch in one transaction, dispatches each event under a
// savepoint, and marks it processed (or reschedules with backoff / dead-letters
// past MaxAttempts) — all committed together, so the FOR UPDATE SKIP LOCKED claim
// gives at-least-once without losing or double-locking rows.
func (w *Worker) drainOnce(ctx context.Context) (int, error) {
	var count int
	err := w.DB.WithTx(ctx, func(tx pgx.Tx) error {
		batch, err := claim(ctx, tx, w.BatchSize)
		if err != nil {
			return err
		}
		count = len(batch)
		for _, e := range batch {
			derr := w.dispatch(ctx, tx, e)
			if derr == nil {
				if _, err := tx.Exec(ctx, "SELECT mark_outbox_processed($1)", e.ID); err != nil {
					return err
				}
				continue
			}
			if e.Attempts+1 >= w.MaxAttempts {
				w.Logger.ErrorContext(ctx, "dropping poison event after max attempts",
					"id", e.ID, "topic", e.Topic, "attempts", e.Attempts, "err", derr)
				if _, err := tx.Exec(ctx, "SELECT mark_outbox_processed($1)", e.ID); err != nil {
					return err
				}
				continue
			}
			w.Logger.WarnContext(ctx, "event handler failed; rescheduling",
				"id", e.ID, "topic", e.Topic, "attempts", e.Attempts, "err", derr)
			if _, err := tx.Exec(ctx,
				"SELECT reschedule_outbox($1, make_interval(secs => $2::int))",
				e.ID, backoffSeconds(e.Attempts)); err != nil {
				return err
			}
		}
		return nil
	})
	return count, err
}

// dispatch runs every subscriber for the event's topic inside a savepoint so a
// handler failure rolls back only that event's DB writes. A topic with no
// subscriber is treated as processed.
func (w *Worker) dispatch(ctx context.Context, tx pgx.Tx, e Event) error {
	handlers := w.Bus.handlers(e.Topic)
	if len(handlers) == 0 {
		return nil
	}
	sp, err := tx.Begin(ctx) // pgx nested Begin == SAVEPOINT
	if err != nil {
		return err
	}
	for _, h := range handlers {
		if herr := h(ctx, sp, e); herr != nil {
			_ = sp.Rollback(ctx)
			return herr
		}
	}
	return sp.Commit(ctx)
}

func claim(ctx context.Context, tx pgx.Tx, limit int) ([]Event, error) {
	rows, err := tx.Query(ctx,
		"SELECT id, tenant_root_id, topic, payload, attempts FROM claim_outbox_batch($1)", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TenantRootID, &e.Topic, &e.Payload, &e.Attempts); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// backoffSeconds is an exponential backoff capped at 300s.
func backoffSeconds(attempts int32) int {
	shift := attempts + 1
	if shift > 8 {
		shift = 8
	}
	s := 1 << shift // 2 .. 256
	if s > 300 {
		s = 300
	}
	return s
}
