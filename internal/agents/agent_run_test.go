package agents

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestMapAgentRunErr(t *testing.T) {
	if got := mapAgentRunErr(nil); got != nil {
		t.Fatalf("nil → %v", got)
	}
	if got := mapAgentRunErr(pgx.ErrNoRows); !errors.Is(got, errs.ErrNotFound) {
		t.Fatalf("ErrNoRows → %v, want ErrNotFound", got)
	}
	sentinel := errors.New("boom")
	if got := mapAgentRunErr(sentinel); !errors.Is(got, sentinel) || errors.Is(got, errs.ErrNotFound) {
		t.Fatalf("opaque err must wrap as 500-class, got %v", got)
	}
}

func TestValidTrigger(t *testing.T) {
	for _, ok := range []string{"event", "manual"} {
		if !validTrigger(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	if validTrigger("cron") {
		t.Fatal("unknown trigger must be rejected")
	}
}

func TestValidStatus(t *testing.T) {
	for _, ok := range []string{RunQueued, RunRunning, RunAwaitingApproval, RunSucceeded, RunFailed} {
		if !validStatus(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	if validStatus("done") {
		t.Fatal("unknown status must be rejected")
	}
}

// TestProgressRejectsBadInputWithoutDB proves Progress validates inputs before
// touching the DB tx — a zero-value store (DB nil) must not panic.
func TestProgressRejectsBadInputWithoutDB(t *testing.T) {
	s := &AgentRunStore{} // DB nil: any DB access would panic
	ctx := context.Background()

	if _, err := s.Progress(ctx, uuid.New(), uuid.New(), uuid.New(), "done", 0, 0, 0, nil); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("bad status → %v, want ErrValidation", err)
	}
	if _, err := s.Progress(ctx, uuid.New(), uuid.New(), uuid.New(), RunRunning, -1, 0, 0, nil); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("negative tokensIn → %v, want ErrValidation", err)
	}
}

// TestCreateRunRejectsBadTriggerWithoutDB proves CreateRun validates the trigger
// before touching the DB tx — a zero-value store (DB nil) must not panic.
func TestCreateRunRejectsBadTriggerWithoutDB(t *testing.T) {
	s := &AgentRunStore{} // DB nil: any DB access would panic
	ctx := context.Background()

	if _, err := s.CreateRun(ctx, uuid.New(), uuid.New(), uuid.New(), "cron", "corr-1", nil, nil); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("bad trigger → %v, want ErrValidation", err)
	}
}
