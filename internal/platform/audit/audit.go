// Package audit writes append-only audit entries within the same transaction as
// the change they record (Constitution Principle VI). The app role has no
// UPDATE/DELETE on audit_entry; erasure is a separate restricted path.
package audit

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// Entry describes an auditable mutation. business_id/tenant_root_id are nil for
// global account/security events.
type Entry struct {
	BusinessID       *uuid.UUID
	TenantRootID     *uuid.UUID
	ActorPrincipalID *uuid.UUID
	Action           string
	TargetType       *string
	TargetID         *uuid.UUID
	CorrelationID    *string
	OldValue         any
	NewValue         any
	// Inputs/Outputs/Decision support agent-run auditing (Spec 003 US3): the
	// tool-call args, the tool result, and the gate/exec decision. Marshalled to
	// the inputs/outputs jsonb + decision text columns. nil ⇒ SQL NULL.
	Inputs   any
	Outputs  any
	Decision *string
}

// marshalJSON returns nil bytes for a nil value (→ SQL NULL), else the JSON encoding.
func marshalJSON(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// Write inserts the entry using tx (so it commits or rolls back with the change).
func Write(ctx context.Context, tx pgx.Tx, e Entry) error {
	oldValue, err := marshalJSON(e.OldValue)
	if err != nil {
		return err
	}
	newValue, err := marshalJSON(e.NewValue)
	if err != nil {
		return err
	}
	inputs, err := marshalJSON(e.Inputs)
	if err != nil {
		return err
	}
	outputs, err := marshalJSON(e.Outputs)
	if err != nil {
		return err
	}
	return dbgen.New(tx).InsertAuditEntry(ctx, dbgen.InsertAuditEntryParams{
		ID:               uuid.New(),
		BusinessID:       db.PGUUIDPtr(e.BusinessID),
		TenantRootID:     db.PGUUIDPtr(e.TenantRootID),
		ActorPrincipalID: db.PGUUIDPtr(e.ActorPrincipalID),
		Action:           e.Action,
		TargetType:       e.TargetType,
		TargetID:         db.PGUUIDPtr(e.TargetID),
		CorrelationID:    e.CorrelationID,
		OldValue:         oldValue,
		NewValue:         newValue,
		Inputs:           inputs,
		Outputs:          outputs,
		Decision:         e.Decision,
	})
}
