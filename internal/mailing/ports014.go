package mailing

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/automations"
)

// AutomationPorts adapts the mailing subsystem to Spec 014's transactional
// ports. The SQL functions are deliberately narrow because the stepper has no
// end-user principal and therefore must not bypass mailing RLS with table reads.
type AutomationPorts struct {
	MessageDomain string
}

func (p AutomationPorts) Snapshot(ctx context.Context, tx pgx.Tx, subscriberID uuid.UUID) (automations.SubscriberSnapshot, error) {
	var snapshot automations.SubscriberSnapshot
	err := tx.QueryRow(ctx, `SELECT subscriber_id,business_id,tenant_root_id,list_id,email,status,tags
		FROM mailing_automation_subscriber_snapshot($1)`, subscriberID).Scan(
		&snapshot.ID, &snapshot.BusinessID, &snapshot.TenantRootID, &snapshot.ListID,
		&snapshot.Email, &snapshot.Status, &snapshot.Tags,
	)
	if err != nil {
		return automations.SubscriberSnapshot{}, err
	}
	return snapshot, nil
}

func (p AutomationPorts) ActiveOnList(ctx context.Context, tx pgx.Tx, businessID uuid.UUID, email string, listID uuid.UUID) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, "SELECT mailing_automation_active_on_list($1,$2,$3)", businessID, email, listID).Scan(&active)
	return active, err
}

func (p AutomationPorts) ResolveForList(ctx context.Context, tx pgx.Tx, businessID uuid.UUID, email string, listID uuid.UUID) (uuid.UUID, error) {
	var value pgtype.UUID
	if err := tx.QueryRow(ctx, "SELECT mailing_automation_resolve_for_list($1,$2,$3)", businessID, email, listID).Scan(&value); err != nil {
		return uuid.Nil, err
	}
	if !value.Valid {
		return uuid.Nil, fmt.Errorf("subscriber was not found: %w", automations.ErrInvalidReference)
	}
	return uuid.UUID(value.Bytes), nil
}

func (p AutomationPorts) Enqueue(ctx context.Context, tx pgx.Tx, spec automations.MessageSpec) (uuid.UUID, error) {
	if spec.SourceKind != "automation" {
		return uuid.Nil, fmt.Errorf("unsupported message source %q: %w", spec.SourceKind, automations.ErrInvalidReference)
	}
	var value pgtype.UUID
	err := tx.QueryRow(ctx, "SELECT mailing_enqueue_delivery($1,$2,$3,$4,$5,$6,$7,$8,$9)",
		spec.BusinessID, spec.TenantRootID, spec.SourceID, spec.TemplateID,
		spec.SubscriberID, spec.NotBefore, safeMessageDomain(p.MessageDomain),
		spec.TrackOpens, spec.TrackClicks,
	).Scan(&value)
	if err != nil {
		return uuid.Nil, err
	}
	if !value.Valid {
		return uuid.Nil, fmt.Errorf("subscriber or template was not found: %w", automations.ErrInvalidReference)
	}
	return uuid.UUID(value.Bytes), nil
}

func (p AutomationPorts) Engagement(ctx context.Context, tx pgx.Tx, deliveryID uuid.UUID) (automations.Engagement, error) {
	var engagement automations.Engagement
	err := tx.QueryRow(ctx, `SELECT opened,clicked_urls
		FROM mailing_automation_delivery_engagement($1)`, deliveryID).Scan(
		&engagement.Opened, &engagement.ClickedURLs,
	)
	if err != nil {
		return automations.Engagement{}, err
	}
	return engagement, nil
}

func (p AutomationPorts) AddTag(ctx context.Context, tx pgx.Tx, businessID, tenantRootID, subscriberID uuid.UUID, tag string) error {
	var changed bool
	err := tx.QueryRow(ctx, "SELECT mailing_automation_add_tag($1,$2,$3,$4)", businessID, tenantRootID, subscriberID, strings.TrimSpace(tag)).Scan(&changed)
	if err == nil && !changed {
		return fmt.Errorf("subscriber was not found: %w", automations.ErrInvalidReference)
	}
	return err
}

func (p AutomationPorts) RemoveTag(ctx context.Context, tx pgx.Tx, businessID, tenantRootID, subscriberID uuid.UUID, tag string) error {
	var found bool
	err := tx.QueryRow(ctx, "SELECT mailing_automation_remove_tag($1,$2,$3,$4)", businessID, tenantRootID, subscriberID, strings.TrimSpace(tag)).Scan(&found)
	if err == nil && !found {
		return fmt.Errorf("subscriber was not found: %w", automations.ErrInvalidReference)
	}
	return err
}

func (p AutomationPorts) TemplateExists(ctx context.Context, tx pgx.Tx, businessID, tenantRootID, templateID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, "SELECT mailing_automation_template_exists($1,$2,$3)", businessID, tenantRootID, templateID).Scan(&exists)
	return exists, err
}

func (p AutomationPorts) ListExists(ctx context.Context, tx pgx.Tx, businessID, tenantRootID, listID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, "SELECT mailing_automation_list_exists($1,$2,$3)", businessID, tenantRootID, listID).Scan(&exists)
	return exists, err
}

var (
	_ automations.SubscriberReader = AutomationPorts{}
	_ automations.MessageSender    = AutomationPorts{}
	_ automations.EngagementReader = AutomationPorts{}
	_ automations.Tagger           = AutomationPorts{}
	_ automations.TemplateReader   = AutomationPorts{}
	_ automations.ListReader       = AutomationPorts{}
)
