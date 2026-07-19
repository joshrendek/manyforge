package agents

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRefresher counts RefreshDue calls and signals on a channel so tests can synchronize
// without relying on wall-clock sleeps.
type fakeRefresher struct {
	calls  int32
	n      int
	err    error
	called chan struct{}
}

func (f *fakeRefresher) RefreshDue(ctx context.Context) (int, error) {
	atomic.AddInt32(&f.calls, 1)
	select {
	case f.called <- struct{}{}:
	default:
	}
	return f.n, f.err
}

// TestCodexRefreshWorker_TicksAndStopsOnCancel asserts RefreshDue is invoked at least once on
// the ticker and that Run returns promptly once ctx is cancelled. Deterministic: cancel as soon
// as one call is observed via the channel, not via a wall-clock sleep bound.
func TestCodexRefreshWorker_TicksAndStopsOnCancel(t *testing.T) {
	f := &fakeRefresher{called: make(chan struct{}, 1)}
	w := &CodexRefreshWorker{
		Svc:    f,
		Logger: slog.Default(),
		Every:  5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	select {
	case <-f.called:
		// at least one sweep observed
	case <-time.After(2 * time.Second):
		t.Fatal("RefreshDue was never invoked")
	}
	cancel()

	select {
	case <-done:
		// Run returned promptly after cancel
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}

	if atomic.LoadInt32(&f.calls) < 1 {
		t.Fatalf("expected at least 1 RefreshDue call, got %d", f.calls)
	}
}

// TestCodexRefreshWorker_ZeroEveryDoesNotPanic asserts Run with a non-positive Every falls back
// to defaultCodexRefreshEvery instead of panicking (time.NewTicker panics on a zero/negative
// interval). The context is cancelled before Run is even started, so a passing Run.C tick can
// never fire: the only way this test returns within the bound is if time.NewTicker did not
// panic and the ctx.Done() branch was taken. Deterministic — no sleep-and-hope.
func TestCodexRefreshWorker_ZeroEveryDoesNotPanic(t *testing.T) {
	f := &fakeRefresher{called: make(chan struct{}, 1)}
	w := &CodexRefreshWorker{
		Svc:    f,
		Logger: slog.Default(),
		Every:  0,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Run starts

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	select {
	case <-done:
		// Run returned promptly without panicking on the zero interval.
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel (or panicked)")
	}
}

// TestCodexRefreshWorker_ErrorDoesNotStopLoop asserts an error from RefreshDue is logged (not
// fatal) and the ticker keeps running — verified by observing a second call after the first.
func TestCodexRefreshWorker_ErrorDoesNotStopLoop(t *testing.T) {
	f := &fakeRefresher{called: make(chan struct{}, 1), err: errors.New("boom")}
	w := &CodexRefreshWorker{
		Svc:    f,
		Logger: slog.Default(),
		Every:  5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	// Observe two sweeps to confirm the loop survives an error from the first.
	for i := 0; i < 2; i++ {
		select {
		case <-f.called:
		case <-time.After(2 * time.Second):
			t.Fatalf("RefreshDue call %d was never observed", i+1)
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
