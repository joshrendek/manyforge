package mailing

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/events"
)

const subscriberCursorKind = "mailing-subscriber"

type subscriberEvent struct {
	BusinessID   uuid.UUID `json:"business_id"`
	TenantRootID uuid.UUID `json:"tenant_root_id"`
	SubscriberID uuid.UUID `json:"subscriber_id"`
	ListID       uuid.UUID `json:"list_id"`
	Email        string    `json:"email"`
	Tag          string    `json:"tag,omitempty"`
	OldStatus    string    `json:"old_status,omitempty"`
	NewStatus    string    `json:"new_status,omitempty"`
}

func (s *Service) CreateSubscriber(ctx context.Context, principalID, businessID, listID uuid.UUID, in SubscriberInput) (Subscriber, error) {
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return Subscriber{}, err
	}
	tags, err := normalizeTags(in.Tags)
	if err != nil {
		return Subscriber{}, err
	}
	attrs, err := jsonObject(in.Attributes)
	if err != nil {
		return Subscriber{}, err
	}
	var out Subscriber
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		list, err := loadActiveList(ctx, q, businessID, root, listID)
		if err != nil {
			return err
		}
		status := dbgen.MailingSubscriberStatusPending
		if in.SkipConfirmation || !list.DoubleOptIn {
			status = dbgen.MailingSubscriberStatusActive
		}
		source := dbgen.MailingConsentSourceManual
		if in.ConsentSource != "" {
			source = dbgen.MailingConsentSource(in.ConsentSource)
		}
		if source != dbgen.MailingConsentSourceManual && source != dbgen.MailingConsentSourceCrm && source != dbgen.MailingConsentSourceCsvImport {
			return validation("invalid consent source")
		}
		row, err := q.InsertListSubscriber(ctx, dbgen.InsertListSubscriberParams{
			ID: uuid.New(), BusinessID: businessID, TenantRootID: root, ListID: listID,
			Email: email, FirstName: cleanOptional(in.FirstName), LastName: cleanOptional(in.LastName), Attributes: attrs,
			Status: status, ContactID: pgUUIDPtr(in.ContactID), ConsentSource: source, ConsentAttestedBy: pgUUIDPtr(&principalID),
		})
		if err != nil {
			return err
		}
		if err := replaceTags(ctx, tx, q, row, tags, nil); err != nil {
			return err
		}
		if status == dbgen.MailingSubscriberStatusActive {
			if err := enqueueSubscriberEvent(ctx, tx, row, events.TopicMailingSubscriberActivated, "", "", ""); err != nil {
				return err
			}
		}
		if err := auditMutation(ctx, tx, principalID, businessID, root, "mailing.subscriber.created", "list_subscriber", row.ID,
			map[string]any{"list_id": listID, "status": status, "consent_source": source, "tag_count": len(tags)}); err != nil {
			return err
		}
		out = toSubscriber(row, tags)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) GetSubscriber(ctx context.Context, principalID, businessID, listID, subscriberID uuid.UUID) (Subscriber, error) {
	var out Subscriber
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err := loadList(ctx, q, businessID, root, listID); err != nil {
			return err
		}
		row, err := loadSubscriber(ctx, q, businessID, root, listID, subscriberID)
		if err != nil {
			return err
		}
		tags, err := loadTags(ctx, q, root, row.ID)
		if err != nil {
			return err
		}
		out = toSubscriber(row, tags)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) ListSubscribers(ctx context.Context, principalID, businessID, listID uuid.UUID, filter SubscriberFilter) (Page[Subscriber], error) {
	lim := clampLimit(filter.Limit)
	if filter.Status != "" && !validSubscriberStatus(filter.Status) {
		return Page[Subscriber]{}, validation("invalid status")
	}
	tag := strings.ToLower(strings.TrimSpace(filter.Tag))
	var out Page[Subscriber]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err := loadList(ctx, q, businessID, root, listID); err != nil {
			return err
		}
		var query, status, tagArg *string
		if v := strings.TrimSpace(filter.Query); v != "" {
			query = &v
		}
		if filter.Status != "" {
			status = &filter.Status
		}
		if tag != "" {
			tagArg = &tag
		}
		var rows []dbgen.ListSubscriber
		if filter.Cursor == "" {
			rows, err = q.ListListSubscribers(ctx, dbgen.ListListSubscribersParams{ListID: listID, TenantRootID: root, Q: query, Status: status, Tag: tagArg, Lim: int32(lim + 1)})
		} else {
			email, id, derr := decodeCursor(subscriberCursorKind, filter.Cursor)
			if derr != nil {
				return derr
			}
			rows, err = q.ListListSubscribersAfter(ctx, dbgen.ListListSubscribersAfterParams{ListID: listID, TenantRootID: root, CurEmail: email, CurID: id, Q: query, Status: status, Tag: tagArg, Lim: int32(lim + 1)})
		}
		if err != nil {
			return err
		}
		more := len(rows) > lim
		if more {
			rows = rows[:lim]
		}
		out.Items = make([]Subscriber, 0, len(rows))
		for _, row := range rows {
			tags, err := loadTags(ctx, q, root, row.ID)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, toSubscriber(row, tags))
		}
		if more {
			last := rows[len(rows)-1]
			out.NextCursor = strPtr(encodeCursor(subscriberCursorKind, last.Email, last.ID))
		}
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) UpdateSubscriber(ctx context.Context, principalID, businessID, listID, subscriberID uuid.UUID, in SubscriberUpdate) (Subscriber, error) {
	var attrs []byte
	var err error
	if in.SetAttributes {
		attrs, err = jsonObject(in.Attributes)
		if err != nil {
			return Subscriber{}, err
		}
	}
	var status dbgen.NullMailingSubscriberStatus
	if in.Status != nil {
		v := strings.ToLower(strings.TrimSpace(*in.Status))
		if !validSubscriberStatus(v) {
			return Subscriber{}, validation("invalid status")
		}
		status = dbgen.NullMailingSubscriberStatus{MailingSubscriberStatus: dbgen.MailingSubscriberStatus(v), Valid: true}
	}
	var newTags []string
	if in.Tags != nil {
		newTags, err = normalizeTags(*in.Tags)
		if err != nil {
			return Subscriber{}, err
		}
	}
	var out Subscriber
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err := loadList(ctx, q, businessID, root, listID); err != nil {
			return err
		}
		before, err := loadSubscriber(ctx, q, businessID, root, listID, subscriberID)
		if err != nil {
			return err
		}
		setFirst, setLast, setReason := in.SetFirstName, in.SetLastName, in.SetStatusReason
		row, err := q.UpdateListSubscriber(ctx, dbgen.UpdateListSubscriberParams{
			SetFirstName: &setFirst, FirstName: cleanOptional(in.FirstName), SetLastName: &setLast, LastName: cleanOptional(in.LastName),
			Attributes: attrs, Status: status, SetStatusReason: &setReason, StatusReason: cleanOptional(in.StatusReason), ID: subscriberID, TenantRootID: root,
		})
		if err != nil {
			return err
		}
		tags, err := loadTags(ctx, q, root, subscriberID)
		if err != nil {
			return err
		}
		if in.Tags != nil {
			old := make(map[string]struct{}, len(tags))
			for _, tag := range tags {
				old[tag] = struct{}{}
			}
			if err := replaceTags(ctx, tx, q, row, newTags, old); err != nil {
				return err
			}
			tags = newTags
		}
		if before.Status != row.Status {
			if err := enqueueSubscriberEvent(ctx, tx, row, events.TopicMailingSubscriberStatusChanged, "", string(before.Status), string(row.Status)); err != nil {
				return err
			}
			if row.Status == dbgen.MailingSubscriberStatusActive {
				if err := enqueueSubscriberEvent(ctx, tx, row, events.TopicMailingSubscriberActivated, "", "", ""); err != nil {
					return err
				}
			}
		}
		if err := auditMutation(ctx, tx, principalID, businessID, root, "mailing.subscriber.updated", "list_subscriber", subscriberID,
			map[string]any{"list_id": listID, "status": row.Status, "tag_count": len(tags)}); err != nil {
			return err
		}
		out = toSubscriber(row, tags)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) UnsubscribeSubscriber(ctx context.Context, principalID, businessID, listID, subscriberID uuid.UUID, reason string) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err := loadList(ctx, q, businessID, root, listID); err != nil {
			return err
		}
		before, err := loadSubscriber(ctx, q, businessID, root, listID, subscriberID)
		if err != nil {
			return err
		}
		row, err := q.UnsubscribeListSubscriber(ctx, dbgen.UnsubscribeListSubscriberParams{ID: subscriberID, TenantRootID: root, StatusReason: strings.TrimSpace(reason)})
		if err != nil {
			return err
		}
		if before.Status != row.Status {
			if err := enqueueSubscriberEvent(ctx, tx, row, events.TopicMailingSubscriberStatusChanged, "", string(before.Status), string(row.Status)); err != nil {
				return err
			}
		}
		return auditMutation(ctx, tx, principalID, businessID, root, "mailing.subscriber.unsubscribed", "list_subscriber", subscriberID, map[string]any{"list_id": listID})
	})
	return mapErr(err)
}

func loadSubscriber(ctx context.Context, q *dbgen.Queries, businessID, root, listID, subscriberID uuid.UUID) (dbgen.ListSubscriber, error) {
	row, err := q.GetListSubscriber(ctx, dbgen.GetListSubscriberParams{ID: subscriberID, TenantRootID: root})
	if err != nil {
		return dbgen.ListSubscriber{}, err
	}
	if row.BusinessID != businessID || row.ListID != listID {
		return dbgen.ListSubscriber{}, pgx.ErrNoRows
	}
	return row, nil
}

func loadTags(ctx context.Context, q *dbgen.Queries, root, subscriberID uuid.UUID) ([]string, error) {
	rows, err := q.ListSubscriberTags(ctx, dbgen.ListSubscriberTagsParams{SubscriberID: subscriberID, TenantRootID: root})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Tag)
	}
	return out, nil
}

func replaceTags(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, row dbgen.ListSubscriber, tags []string, old map[string]struct{}) error {
	if err := q.DeleteSubscriberTags(ctx, dbgen.DeleteSubscriberTagsParams{SubscriberID: row.ID, TenantRootID: row.TenantRootID}); err != nil {
		return err
	}
	for _, tag := range tags {
		if _, err := q.InsertSubscriberTag(ctx, dbgen.InsertSubscriberTagParams{ID: uuid.New(), BusinessID: row.BusinessID, TenantRootID: row.TenantRootID, ListID: row.ListID, SubscriberID: row.ID, Tag: tag}); err != nil {
			return err
		}
		_, existed := old[tag]
		if old == nil || !existed {
			if err := enqueueSubscriberEvent(ctx, tx, row, events.TopicMailingSubscriberTagAdded, tag, "", ""); err != nil {
				return err
			}
		}
	}
	return nil
}

func enqueueSubscriberEvent(ctx context.Context, tx pgx.Tx, row dbgen.ListSubscriber, topic, tag, oldStatus, newStatus string) error {
	return events.Enqueue(ctx, tx, row.TenantRootID, topic, subscriberEvent{BusinessID: row.BusinessID, TenantRootID: row.TenantRootID, SubscriberID: row.ID, ListID: row.ListID, Email: row.Email, Tag: tag, OldStatus: oldStatus, NewStatus: newStatus})
}

func cleanOptional(v *string) *string {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(*v)
	if s == "" {
		return nil
	}
	return &s
}
