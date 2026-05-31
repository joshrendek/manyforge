//go:build integration

package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// TestOutboxDrainAtLeastOnce proves the SL-C contract: events enqueued in a tx are
// claimed, dispatched to subscribers, and marked processed exactly once per drain.
func TestOutboxDrainAtLeastOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	bus := NewBus()
	var mu sync.Mutex
	seen := map[uuid.UUID]int{}
	bus.Subscribe("test.topic", func(_ context.Context, _ pgx.Tx, e Event) error {
		mu.Lock()
		seen[e.ID]++
		mu.Unlock()
		return nil
	})

	tenant := uuid.New()
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		for i := 0; i < 3; i++ {
			if err := Enqueue(ctx, tx, tenant, "test.topic", map[string]any{"n": i}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	w := &Worker{DB: tdb.App, Bus: bus, BatchSize: 10}
	w.withDefaults()

	n, err := w.drainOnce(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 3 {
		t.Fatalf("drained %d, want 3", n)
	}
	mu.Lock()
	distinct := len(seen)
	mu.Unlock()
	if distinct != 3 {
		t.Fatalf("handler saw %d distinct events, want 3", distinct)
	}

	// A second drain finds nothing: all three were marked processed.
	n2, err := w.drainOnce(ctx)
	if err != nil {
		t.Fatalf("drain2: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second drain claimed %d, want 0 (already processed)", n2)
	}
}

// TestOutboxRescheduleOnHandlerError proves a failing handler reschedules its
// event with backoff (not lost, not immediately re-claimed, not marked processed).
func TestOutboxRescheduleOnHandlerError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })

	bus := NewBus()
	bus.Subscribe("flaky", func(_ context.Context, _ pgx.Tx, _ Event) error {
		return errors.New("boom")
	})

	tenant := uuid.New()
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return Enqueue(ctx, tx, tenant, "flaky", map[string]any{})
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	w := &Worker{DB: tdb.App, Bus: bus, BatchSize: 10}
	w.withDefaults()

	n, err := w.drainOnce(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 1 {
		t.Fatalf("drained %d, want 1", n)
	}

	// The rescheduled event has a future available_at, so an immediate re-drain
	// claims nothing, and it is still pending (not marked processed).
	n2, err := w.drainOnce(ctx)
	if err != nil {
		t.Fatalf("drain2: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("rescheduled event re-claimed too early: got %d, want 0", n2)
	}

	var pending, attempts int
	if err := tdb.Super.QueryRow(ctx,
		"SELECT count(*), coalesce(max(attempts),0) FROM outbox WHERE processed_at IS NULL").
		Scan(&pending, &attempts); err != nil {
		t.Fatalf("query pending: %v", err)
	}
	if pending != 1 {
		t.Fatalf("expected 1 pending (rescheduled), got %d", pending)
	}
	if attempts != 1 {
		t.Fatalf("expected attempts=1 after one failure, got %d", attempts)
	}
}
