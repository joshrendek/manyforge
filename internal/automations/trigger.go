package automations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/events"
)

type TriggerSubscriber struct {
	Subscribers SubscriberReader
	Now         func() time.Time
}

type subscriberTriggerPayload struct {
	BusinessID   uuid.UUID `json:"business_id"`
	TenantRootID uuid.UUID `json:"tenant_root_id"`
	SubscriberID uuid.UUID `json:"subscriber_id"`
	ListID       uuid.UUID `json:"list_id"`
	Tag          string    `json:"tag"`
	NewStatus    string    `json:"new_status"`
}

type customEventTriggerPayload struct {
	BusinessID   uuid.UUID  `json:"business_id"`
	TenantRootID uuid.UUID  `json:"tenant_root_id"`
	EventID      uuid.UUID  `json:"event_id"`
	Name         string     `json:"name"`
	Email        string     `json:"email"`
	ListID       *uuid.UUID `json:"list_id"`
}

func (s TriggerSubscriber) Handle(ctx context.Context, tx pgx.Tx, event events.Event) error {
	switch event.Topic {
	case events.TopicMailingSubscriberActivated, events.TopicMailingSubscriberTagAdded, events.TopicMailingSubscriberStatusChanged:
		return s.handleSubscriber(ctx, tx, event)
	case events.TopicAutomationEventReceived:
		return s.handleCustomEvent(ctx, tx, event)
	default:
		return nil
	}
}

func (s TriggerSubscriber) handleSubscriber(ctx context.Context, tx pgx.Tx, event events.Event) error {
	var payload subscriberTriggerPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode %s payload: %w", event.Topic, err)
	}
	if payload.TenantRootID != event.TenantRootID || payload.BusinessID == uuid.Nil || payload.SubscriberID == uuid.Nil || payload.ListID == uuid.Nil {
		return fmt.Errorf("invalid %s tenant payload", event.Topic)
	}
	if event.Topic == events.TopicMailingSubscriberStatusChanged {
		if payload.NewStatus == "active" {
			return nil
		}
		var exited int
		return tx.QueryRow(ctx, "SELECT automation_exit_for_subscriber($1,$2,$3)", payload.SubscriberID, payload.TenantRootID,
			statusExitReason(payload.NewStatus)).Scan(&exited)
	}
	triggerKind, triggerRef := "list_joined", payload.ListID.String()
	if event.Topic == events.TopicMailingSubscriberTagAdded {
		triggerKind, triggerRef = "tag_added", strings.ToLower(strings.TrimSpace(payload.Tag))
		if triggerRef == "" {
			return errors.New("tag_added payload has no tag")
		}
	}
	return enrollForTrigger(ctx, tx, payload.BusinessID, payload.TenantRootID, triggerKind, triggerRef, payload.SubscriberID, event.ID, s.now())
}

func (s TriggerSubscriber) handleCustomEvent(ctx context.Context, tx pgx.Tx, event events.Event) error {
	if s.Subscribers == nil {
		return errors.New("automation trigger subscriber reader is not configured")
	}
	var payload customEventTriggerPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode automation event payload: %w", err)
	}
	if payload.TenantRootID != event.TenantRootID || payload.BusinessID == uuid.Nil || payload.EventID == uuid.Nil || strings.TrimSpace(payload.Name) == "" || strings.TrimSpace(payload.Email) == "" {
		return errors.New("invalid automation event tenant payload")
	}
	rows, err := tx.Query(ctx, `SELECT list_id FROM automation_event_trigger_lists($1,$2,$3,$4)`,
		payload.BusinessID, payload.TenantRootID, payload.Name, payload.ListID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var listIDs []uuid.UUID
	for rows.Next() {
		var listID uuid.UUID
		if err = rows.Scan(&listID); err != nil {
			return err
		}
		listIDs = append(listIDs, listID)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, listID := range listIDs {
		subscriberID, resolveErr := s.Subscribers.ResolveForList(ctx, tx, payload.BusinessID, payload.Email, listID)
		if errors.Is(resolveErr, ErrInvalidReference) {
			continue
		}
		if resolveErr != nil {
			return resolveErr
		}
		if err = enrollForTrigger(ctx, tx, payload.BusinessID, payload.TenantRootID, "event", payload.Name, subscriberID, event.ID, s.now()); err != nil {
			return err
		}
	}
	return nil
}

func enrollForTrigger(ctx context.Context, tx pgx.Tx, businessID, tenantRootID uuid.UUID, kind, ref string, subscriberID, sourceEventID uuid.UUID, now time.Time) error {
	var enrolled int
	return tx.QueryRow(ctx, `SELECT automation_enroll_for_trigger($1,$2,$3,$4,$5,$6,$7)`,
		businessID, tenantRootID, kind, ref, subscriberID, sourceEventID, now).Scan(&enrolled)
}

func statusExitReason(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return "subscriber_inactive"
	}
	return status
}

func (s TriggerSubscriber) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
