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

type EnrollmentView struct {
	ID            uuid.UUID  `json:"id"`
	BusinessID    uuid.UUID  `json:"business_id"`
	TenantRootID  uuid.UUID  `json:"tenant_root_id"`
	AutomationID  uuid.UUID  `json:"automation_id"`
	VersionID     uuid.UUID  `json:"version_id"`
	SubscriberID  uuid.UUID  `json:"subscriber_id"`
	Status        string     `json:"status"`
	CurrentNodeID *string    `json:"current_node_id"`
	WakeAt        *time.Time `json:"wake_at"`
	NodeAttempts  int32      `json:"node_attempts"`
	LastError     *string    `json:"last_error"`
	ExitReason    *string    `json:"exit_reason"`
	SourceEventID *uuid.UUID `json:"source_event_id"`
	EnrolledAt    time.Time  `json:"enrolled_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type EnrollmentStepView struct {
	ID          uuid.UUID       `json:"id"`
	NodeID      string          `json:"node_id"`
	NodeKind    string          `json:"node_kind"`
	Attempt     int32           `json:"attempt"`
	EnteredAt   time.Time       `json:"entered_at"`
	CompletedAt *time.Time      `json:"completed_at"`
	Outcome     string          `json:"outcome"`
	DeliveryID  *uuid.UUID      `json:"delivery_id"`
	Detail      json.RawMessage `json:"detail"`
}

type EnrollmentDetail struct {
	Enrollment EnrollmentView       `json:"enrollment"`
	Steps      []EnrollmentStepView `json:"steps"`
}

type EnrollmentFilter struct {
	Status string
	NodeID string
	Cursor string
	Limit  int
}

type EventInput struct {
	Name           string         `json:"name"`
	Email          *string        `json:"email"`
	SubscriberID   *uuid.UUID     `json:"subscriber_id"`
	OccurredAt     *time.Time     `json:"occurred_at"`
	IdempotencyKey *string        `json:"idempotency_key"`
	Properties     map[string]any `json:"properties"`
}

type EventView struct {
	ID             uuid.UUID       `json:"id"`
	BusinessID     uuid.UUID       `json:"business_id"`
	Name           string          `json:"name"`
	Email          string          `json:"email"`
	SubscriberID   *uuid.UUID      `json:"subscriber_id"`
	OccurredAt     time.Time       `json:"occurred_at"`
	IdempotencyKey *string         `json:"idempotency_key"`
	Properties     json.RawMessage `json:"properties"`
	CreatedAt      time.Time       `json:"created_at"`
	Created        bool            `json:"created"`
}

type EnrollmentCounts struct {
	Active    int64 `json:"active"`
	Completed int64 `json:"completed"`
	Exited    int64 `json:"exited"`
	Errored   int64 `json:"errored"`
}

type NodeStats struct {
	NodeID    string `json:"node_id"`
	NodeKind  string `json:"node_kind"`
	Entered   int64  `json:"entered"`
	Waiting   int64  `json:"waiting"`
	Advanced  int64  `json:"advanced"`
	Sent      int64  `json:"sent"`
	Opened    int64  `json:"opened"`
	Clicked   int64  `json:"clicked"`
	BranchYes int64  `json:"branch_yes"`
	BranchNo  int64  `json:"branch_no"`
	Exited    int64  `json:"exited"`
	Errors    int64  `json:"errors"`
}

type Stats struct {
	AutomationID uuid.UUID        `json:"automation_id"`
	VersionID    uuid.UUID        `json:"version_id"`
	Enrollments  EnrollmentCounts `json:"enrollments"`
	Nodes        []NodeStats      `json:"nodes"`
}

func marshalGraph(graph Graph) ([]byte, error) { return json.Marshal(graph) }
