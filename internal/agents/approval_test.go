package agents

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

func TestApprovalTTLDefault(t *testing.T) {
	s := &ApprovalStore{}
	if s.ttl() != defaultApprovalTTL {
		t.Fatalf("ttl=%v want %v", s.ttl(), defaultApprovalTTL)
	}
	s.TTL = time.Hour
	if s.ttl() != time.Hour {
		t.Fatalf("override ttl=%v want 1h", s.ttl())
	}
	s.TTL = -1 // non-positive falls back to the default
	if s.ttl() != defaultApprovalTTL {
		t.Fatalf("non-positive ttl=%v want default %v", s.ttl(), defaultApprovalTTL)
	}
}

// TestToApprovalItemMapping pins the nullable-field handling in toApprovalItem:
// an unset (NULL) decided_by/executed_at maps to nil pointers; a valued one maps
// through .Valid/.Bytes/.Time correctly. error is *string passed straight through.
func TestToApprovalItemMapping(t *testing.T) {
	id := uuid.New()
	runID := uuid.New()
	bizID := uuid.New()
	rootID := uuid.New()
	expires := time.Now().Add(24 * time.Hour).UTC()

	t.Run("null nullable fields", func(t *testing.T) {
		row := dbgen.ApprovalItem{
			ID: id, AgentRunID: runID, BusinessID: bizID, TenantRootID: rootID,
			Tool: "draft_reply", Args: []byte(`{"body":"hi"}`), EffectClass: 2,
			State:                ApprovalPending,
			DecidedByPrincipalID: pgtype.UUID{Valid: false},
			ExecutedAt:           pgtype.Timestamptz{Valid: false},
			ExpiresAt:            expires,
			Error:                nil,
		}
		got := toApprovalItem(row)
		if got.ID != id || got.AgentRunID != runID || got.BusinessID != bizID || got.TenantRootID != rootID {
			t.Fatalf("id fields mismatch: %+v", got)
		}
		if got.Tool != "draft_reply" || got.EffectClass != 2 || got.State != ApprovalPending {
			t.Fatalf("scalar fields mismatch: %+v", got)
		}
		if string(got.Args) != `{"body":"hi"}` {
			t.Fatalf("args=%s", got.Args)
		}
		if got.DecidedByPrincipalID != nil {
			t.Fatalf("decided_by should be nil, got %v", *got.DecidedByPrincipalID)
		}
		if got.ExecutedAt != nil {
			t.Fatalf("executed_at should be nil, got %v", *got.ExecutedAt)
		}
		if got.Error != nil {
			t.Fatalf("error should be nil, got %v", *got.Error)
		}
		if !got.ExpiresAt.Equal(expires) {
			t.Fatalf("expires=%v want %v", got.ExpiresAt, expires)
		}
	})

	t.Run("valued nullable fields", func(t *testing.T) {
		decider := uuid.New()
		execAt := time.Now().Add(-time.Minute).UTC()
		errMsg := "boom"
		row := dbgen.ApprovalItem{
			ID: id, AgentRunID: runID, BusinessID: bizID, TenantRootID: rootID,
			Tool: "set_status", Args: []byte(`{}`), EffectClass: 1,
			State:                ApprovalExecuted,
			DecidedByPrincipalID: pgtype.UUID{Bytes: decider, Valid: true},
			ExecutedAt:           pgtype.Timestamptz{Time: execAt, Valid: true},
			ExpiresAt:            expires,
			Error:                &errMsg,
		}
		got := toApprovalItem(row)
		if got.DecidedByPrincipalID == nil || *got.DecidedByPrincipalID != decider {
			t.Fatalf("decided_by mapping wrong: %+v", got.DecidedByPrincipalID)
		}
		if got.ExecutedAt == nil || !got.ExecutedAt.Equal(execAt) {
			t.Fatalf("executed_at mapping wrong: %+v", got.ExecutedAt)
		}
		if got.Error == nil || *got.Error != "boom" {
			t.Fatalf("error mapping wrong: %+v", got.Error)
		}
		if got.State != ApprovalExecuted || got.EffectClass != 1 {
			t.Fatalf("scalar mismatch: %+v", got)
		}
	})
}

// guard: ensure ApprovalItem JSON args round-trip as raw JSON (no double-encoding).
func TestApprovalItemArgsRaw(t *testing.T) {
	row := dbgen.ApprovalItem{Args: []byte(`{"x":1}`), ExpiresAt: time.Now()}
	got := toApprovalItem(row)
	var m map[string]int
	if err := json.Unmarshal(got.Args, &m); err != nil {
		t.Fatalf("args not valid json: %v", err)
	}
	if m["x"] != 1 {
		t.Fatalf("args decoded wrong: %v", m)
	}
}
