package automations

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Automation struct {
	ID                   uuid.UUID  `json:"id"`
	BusinessID           uuid.UUID  `json:"business_id"`
	TenantRootID         uuid.UUID  `json:"tenant_root_id"`
	Name                 string     `json:"name"`
	Description          *string    `json:"description"`
	Status               string     `json:"status"`
	AllowReenroll        bool       `json:"allow_reenroll"`
	ActiveVersionID      *uuid.UUID `json:"active_version_id"`
	DraftVersionID       *uuid.UUID `json:"draft_version_id"`
	CreatedByPrincipalID *uuid.UUID `json:"created_by_principal_id"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type Version struct {
	ID           uuid.UUID  `json:"id"`
	BusinessID   uuid.UUID  `json:"business_id"`
	TenantRootID uuid.UUID  `json:"tenant_root_id"`
	AutomationID uuid.UUID  `json:"automation_id"`
	Number       int32      `json:"number"`
	Status       string     `json:"status"`
	Graph        Graph      `json:"graph"`
	TriggerKind  *string    `json:"trigger_kind"`
	TriggerRef   *string    `json:"trigger_ref"`
	ActivatedAt  *time.Time `json:"activated_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type CreateInput struct {
	Name          string
	Description   *string
	AllowReenroll bool
}

type UpdateInput struct {
	Name           *string
	SetDescription bool
	Description    *string
	AllowReenroll  *bool
}

type ValidationResult struct {
	Valid  bool    `json:"valid"`
	Issues []Issue `json:"issues"`
}

type InvalidGraphError struct{ Issues []Issue }

func (e *InvalidGraphError) Error() string { return "automation graph is invalid" }

func marshalGraph(graph Graph) ([]byte, error) { return json.Marshal(graph) }
