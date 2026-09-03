package automations

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrInvalidReference marks a runtime validation failure, such as a template
// deleted after a version was validated. These failures are terminal because a
// retry cannot repair the pinned graph.
var ErrInvalidReference = errors.New("automation invalid reference")

type SubscriberSnapshot struct {
	ID, BusinessID, TenantRootID, ListID uuid.UUID
	Email                                string
	Status                               string
	Tags                                 []string
}

type SubscriberReader interface {
	Snapshot(context.Context, pgx.Tx, uuid.UUID) (SubscriberSnapshot, error)
	ActiveOnList(context.Context, pgx.Tx, uuid.UUID, string, uuid.UUID) (bool, error)
	ResolveForList(context.Context, pgx.Tx, uuid.UUID, string, uuid.UUID) (uuid.UUID, error)
}

type MessageSpec struct {
	BusinessID, TenantRootID, SubscriberID, TemplateID uuid.UUID
	TrackOpens, TrackClicks                            bool
	SourceKind                                         string
	SourceID                                           uuid.UUID
	NotBefore                                          time.Time
}

type MessageSender interface {
	Enqueue(context.Context, pgx.Tx, MessageSpec) (uuid.UUID, error)
}

type Engagement struct {
	Opened      bool
	ClickedURLs []string
}

type EngagementReader interface {
	Engagement(context.Context, pgx.Tx, uuid.UUID) (Engagement, error)
}

type Tagger interface {
	AddTag(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, string) error
	RemoveTag(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, string) error
}

type TemplateReader interface {
	TemplateExists(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
}

type ListReader interface {
	ListExists(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
}

type StepRecord struct {
	EnrollmentID    uuid.UUID
	ClaimGeneration int
	NodeID          string
	NodeKind        string
	Outcome         string
	NextNodeID      *string
	WakeAt          *time.Time
	Status          string
	DeliveryID      *uuid.UUID
	Detail          map[string]any
	RecordedAt      time.Time
}

type StepFailure struct {
	EnrollmentID    uuid.UUID
	ClaimGeneration int
	Error           string
	Terminal        bool
	RetryAt         time.Time
}

// StepStore is the narrow persistence boundary used by the pure execution
// engine. Implementations must use the caller's transaction.
type StepStore interface {
	Record(context.Context, pgx.Tx, StepRecord) (bool, error)
	Fail(context.Context, pgx.Tx, StepFailure) (bool, error)
	Waiting(context.Context, pgx.Tx, uuid.UUID, string) (bool, error)
	Delivery(context.Context, pgx.Tx, uuid.UUID, string) (*uuid.UUID, error)
	EventExists(context.Context, pgx.Tx, uuid.UUID, string, string, time.Time, *time.Duration) (bool, error)
}

type Deps struct {
	Subscribers SubscriberReader
	Sender      MessageSender
	Engagement  EngagementReader
	Tagger      Tagger
	Templates   TemplateReader
	Lists       ListReader
	Steps       StepStore
}
