package mailing

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mailrender "github.com/manyforge/manyforge/internal/mailing/render"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

const (
	campaignCursorKind = "mailing-campaign"
	deliveryCursorKind = "mailing-delivery"
)

var messageDomainPattern = regexp.MustCompile(`^[a-z0-9.-]+$`)

func validateCampaign(name, subject, body string, listID uuid.UUID) error {
	if listID == uuid.Nil {
		return validation("list_id is required")
	}
	if strings.TrimSpace(name) == "" || len(strings.TrimSpace(name)) > 200 {
		return validation("campaign name is required and must not exceed 200 characters")
	}
	if len(strings.TrimSpace(subject)) > 500 {
		return validation("subject must not exceed 500 characters")
	}
	if len(body) > 1<<20 {
		return validation("body_markdown exceeds 1 MiB")
	}
	return nil
}

func (s *Service) CreateCampaign(ctx context.Context, principalID, businessID uuid.UUID, in CampaignInput) (Campaign, error) {
	in.Name, in.Subject = strings.TrimSpace(in.Name), strings.TrimSpace(in.Subject)
	if err := validateCampaign(in.Name, in.Subject, in.BodyMarkdown, in.ListID); err != nil {
		return Campaign{}, err
	}
	tags, err := normalizeTags(in.TagFilter)
	if err != nil {
		return Campaign{}, err
	}
	var out Campaign
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err = loadList(ctx, q, businessID, root, in.ListID); err != nil {
			return err
		}
		row, err := q.InsertCampaign(ctx, dbgen.InsertCampaignParams{
			ID: uuid.New(), BusinessID: businessID, TenantRootID: root, ListID: in.ListID,
			Name: in.Name, Subject: in.Subject, Preheader: cleanOptional(in.Preheader),
			BodyMarkdown: in.BodyMarkdown, TagFilter: tags, TrackOpens: in.TrackOpens,
			TrackClicks: in.TrackClicks, CreatedBy: pgUUIDPtr(&principalID),
		})
		if err != nil {
			return err
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root,
			"mailing.campaign.created", "campaign", row.ID,
			map[string]any{"list_id": row.ListID, "tag_count": len(tags)}); err != nil {
			return err
		}
		out = toCampaign(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) GetCampaign(ctx context.Context, principalID, businessID, campaignID uuid.UUID) (Campaign, error) {
	var out Campaign
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := loadCampaign(ctx, q, businessID, root, campaignID)
		if err != nil {
			return err
		}
		out = toCampaign(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) ListCampaigns(ctx context.Context, principalID, businessID uuid.UUID, cursor string, limit int) (Page[Campaign], error) {
	lim := clampLimit(limit)
	var out Page[Campaign]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		var rows []dbgen.Campaign
		if cursor == "" {
			rows, err = q.ListCampaigns(ctx, dbgen.ListCampaignsParams{BusinessID: businessID, TenantRootID: root, Limit: int32(lim + 1)})
		} else {
			at, id, derr := decodeTimeCursor(campaignCursorKind, cursor)
			if derr != nil {
				return derr
			}
			rows, err = q.ListCampaignsAfter(ctx, dbgen.ListCampaignsAfterParams{BusinessID: businessID, TenantRootID: root, CurCreated: at, CurID: id, Lim: int32(lim + 1)})
		}
		if err != nil {
			return err
		}
		more := len(rows) > lim
		if more {
			rows = rows[:lim]
		}
		out.Items = make([]Campaign, 0, len(rows))
		for _, row := range rows {
			out.Items = append(out.Items, toCampaign(row))
		}
		if more {
			last := rows[len(rows)-1]
			out.NextCursor = strPtr(encodeTimeCursor(campaignCursorKind, last.CreatedAt, last.ID))
		}
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) UpdateCampaign(ctx context.Context, principalID, businessID, campaignID uuid.UUID, in CampaignUpdate) (Campaign, error) {
	var out Campaign
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		current, err := loadCampaign(ctx, q, businessID, root, campaignID)
		if err != nil {
			return err
		}
		if current.Status != dbgen.CampaignStatusDraft {
			return fmt.Errorf("mailing: only draft campaigns may be edited: %w", errs.ErrConflict)
		}
		listID, name, subject := current.ListID, current.Name, current.Subject
		preheader, body := current.Preheader, current.BodyMarkdown
		tags := current.TagFilter
		trackOpens, trackClicks := current.TrackOpens, current.TrackClicks
		if in.ListID != nil {
			listID = *in.ListID
		}
		if in.Name != nil {
			name = strings.TrimSpace(*in.Name)
		}
		if in.Subject != nil {
			subject = strings.TrimSpace(*in.Subject)
		}
		if in.SetPreheader {
			preheader = cleanOptional(in.Preheader)
		}
		if in.BodyMarkdown != nil {
			body = *in.BodyMarkdown
		}
		if in.TagFilter != nil {
			tags, err = normalizeTags(*in.TagFilter)
			if err != nil {
				return err
			}
		}
		if in.TrackOpens != nil {
			trackOpens = *in.TrackOpens
		}
		if in.TrackClicks != nil {
			trackClicks = *in.TrackClicks
		}
		if err = validateCampaign(name, subject, body, listID); err != nil {
			return err
		}
		if _, err = loadList(ctx, q, businessID, root, listID); err != nil {
			return err
		}
		row, err := q.UpdateCampaign(ctx, dbgen.UpdateCampaignParams{
			ListID: listID, Name: name, Subject: subject, Preheader: preheader,
			BodyMarkdown: body, TagFilter: tags, TrackOpens: trackOpens,
			TrackClicks: trackClicks, ID: campaignID, TenantRootID: root,
		})
		if err != nil {
			return err
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root,
			"mailing.campaign.updated", "campaign", campaignID,
			map[string]any{"list_id": row.ListID, "tag_count": len(tags)}); err != nil {
			return err
		}
		out = toCampaign(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) DeleteCampaign(ctx context.Context, principalID, businessID, campaignID uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		current, err := loadCampaign(ctx, q, businessID, root, campaignID)
		if err != nil {
			return err
		}
		if current.Status != dbgen.CampaignStatusDraft && current.Status != dbgen.CampaignStatusCancelled {
			return fmt.Errorf("mailing: only draft or cancelled campaigns may be deleted: %w", errs.ErrConflict)
		}
		if _, err = q.DeleteCampaign(ctx, dbgen.DeleteCampaignParams{ID: campaignID, TenantRootID: root}); err != nil {
			return err
		}
		return auditMutation(ctx, tx, principalID, businessID, root,
			"mailing.campaign.deleted", "campaign", campaignID, map[string]any{"status": current.Status})
	})
	return mapErr(err)
}

func (s *Service) SendCampaign(ctx context.Context, principalID, businessID, campaignID uuid.UUID, scheduledAt *time.Time) (Campaign, error) {
	at := s.now()
	if scheduledAt != nil && scheduledAt.After(at) {
		at = *scheduledAt
	}
	var out Campaign
	postalAddressWarning := false
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		current, err := loadCampaign(ctx, q, businessID, root, campaignID)
		if err != nil {
			return err
		}
		if current.Status != dbgen.CampaignStatusDraft {
			return fmt.Errorf("mailing: campaign is not a draft: %w", errs.ErrConflict)
		}
		if strings.TrimSpace(current.Subject) == "" || strings.TrimSpace(current.BodyMarkdown) == "" {
			return validation("subject and body_markdown are required before sending")
		}
		row, err := q.ScheduleCampaign(ctx, dbgen.ScheduleCampaignParams{ScheduledAt: at, ID: campaignID, TenantRootID: root, BusinessID: businessID})
		if errors.Is(err, pgx.ErrNoRows) {
			return validation("an active list and verified sending profile are required")
		}
		if err != nil {
			return err
		}
		profile, err := q.GetMailingSendingProfile(ctx, dbgen.GetMailingSendingProfileParams{BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		postalAddressWarning = strings.TrimSpace(stringValue(profile.PostalAddress)) == ""
		if err = auditMutation(ctx, tx, principalID, businessID, root,
			"mailing.campaign.scheduled", "campaign", campaignID,
			map[string]any{"scheduled_at": at, "postal_address_warning": postalAddressWarning}); err != nil {
			return err
		}
		out = toCampaign(row)
		return nil
	})
	if err == nil && postalAddressWarning && s.Logger != nil {
		s.Logger.WarnContext(ctx, "mailing campaign scheduled without a postal address", "campaign_id", campaignID, "business_id", businessID)
	}
	return out, mapErr(err)
}

func (s *Service) CancelCampaign(ctx context.Context, principalID, businessID, campaignID uuid.UUID) (Campaign, error) {
	var out Campaign
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		current, err := loadCampaign(ctx, q, businessID, root, campaignID)
		if err != nil {
			return err
		}
		if current.Status != dbgen.CampaignStatusScheduled && current.Status != dbgen.CampaignStatusSending {
			return fmt.Errorf("mailing: campaign cannot be cancelled: %w", errs.ErrConflict)
		}
		var changed bool
		if err = tx.QueryRow(ctx, "SELECT mailing_cancel_campaign($1)", campaignID).Scan(&changed); err != nil {
			return err
		}
		if !changed {
			return fmt.Errorf("mailing: campaign could not be cancelled: %w", errs.ErrConflict)
		}
		row, err := loadCampaign(ctx, q, businessID, root, campaignID)
		if err != nil {
			return err
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root,
			"mailing.campaign.cancelled", "campaign", campaignID, map[string]any{}); err != nil {
			return err
		}
		out = toCampaign(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) CampaignStats(ctx context.Context, principalID, businessID, campaignID uuid.UUID) (CampaignStats, error) {
	var out CampaignStats
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := loadCampaign(ctx, q, businessID, root, campaignID)
		if err != nil {
			return err
		}
		links, err := q.CampaignLinkStats(ctx, dbgen.CampaignLinkStatsParams{CampaignID: pgUUIDPtr(&campaignID), TenantRootID: root})
		if err != nil {
			return err
		}
		out.Campaign = toCampaign(row)
		out.Links = make([]CampaignLinkStat, 0, len(links))
		for _, link := range links {
			out.Links = append(out.Links, CampaignLinkStat{URL: stringValue(link.Url), ClickCount: link.ClickCount, UniqueClickCount: link.UniqueClickCount})
		}
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) ListCampaignDeliveries(ctx context.Context, principalID, businessID, campaignID uuid.UUID, status, cursor string, limit int) (Page[Delivery], error) {
	if status != "" && !validDeliveryStatus(status) {
		return Page[Delivery]{}, validation("invalid delivery status")
	}
	lim := clampLimit(limit)
	var out Page[Delivery]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err = loadCampaign(ctx, q, businessID, root, campaignID); err != nil {
			return err
		}
		var rows []dbgen.MailingDelivery
		if cursor == "" {
			rows, err = q.ListCampaignDeliveries(ctx, dbgen.ListCampaignDeliveriesParams{CampaignID: pgUUIDPtr(&campaignID), TenantRootID: root, Status: status, Lim: int32(lim + 1)})
		} else {
			at, id, derr := decodeTimeCursor(deliveryCursorKind, cursor)
			if derr != nil {
				return derr
			}
			rows, err = q.ListCampaignDeliveriesAfter(ctx, dbgen.ListCampaignDeliveriesAfterParams{CampaignID: pgUUIDPtr(&campaignID), TenantRootID: root, Status: status, CurCreated: at, CurID: id, Lim: int32(lim + 1)})
		}
		if err != nil {
			return err
		}
		more := len(rows) > lim
		if more {
			rows = rows[:lim]
		}
		out.Items = make([]Delivery, 0, len(rows))
		for _, row := range rows {
			out.Items = append(out.Items, toDelivery(row))
		}
		if more {
			last := rows[len(rows)-1]
			out.NextCursor = strPtr(encodeTimeCursor(deliveryCursorKind, last.CreatedAt, last.ID))
		}
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) TestCampaign(ctx context.Context, principalID, businessID, campaignID uuid.UUID, recipients []string) error {
	if len(recipients) == 0 || len(recipients) > 5 {
		return validation("test-send requires between 1 and 5 recipients")
	}
	clean := make([]string, len(recipients))
	for i, recipient := range recipients {
		var err error
		clean[i], err = normalizeEmail(recipient)
		if err != nil {
			return err
		}
	}
	campaign, err := s.GetCampaign(ctx, principalID, businessID, campaignID)
	if err != nil {
		return err
	}
	profile, providerProfile, err := s.loadProviderProfile(ctx, principalID, businessID)
	if err != nil {
		return err
	}
	if profile.Status != "verified" {
		return validation("sending profile must be verified")
	}
	if s.Providers == nil || s.Renderer == nil {
		return errors.New("mailing: delivery is not configured")
	}
	deliverer, err := s.Providers.Resolve(ctx, providerProfile)
	if err != nil {
		return fmt.Errorf("mailing: resolve provider: %w", errs.ErrUpstream)
	}
	compiled, err := s.Renderer.Compile(mailrender.Input{BodyMarkdown: campaign.BodyMarkdown,
		FromName: profile.FromName, Preheader: stringValue(campaign.Preheader),
		PostalAddress: stringValue(profile.PostalAddress)})
	if err != nil {
		return err
	}
	for _, recipient := range clean {
		if s.OutboundLimiter != nil && !s.OutboundLimiter.Allow("ob:biz:"+businessID.String()) {
			return fmt.Errorf("mailing: outbound rate limit: %w", errs.ErrRateLimited)
		}
		rendered, err := s.Renderer.Render(compiled, mailrender.Variables{Email: recipient,
			UnsubscribeURL: "#", ListName: "Test campaign"}, mailrender.Tracking{})
		if err != nil {
			return err
		}
		message := notify.Mail{From: (&mail.Address{Name: profile.FromName, Address: profile.FromEmail}).String(),
			To: recipient, Subject: "[TEST] " + campaign.Subject, BodyText: rendered.Text,
			BodyHTML: rendered.HTML, MessageID: uuid.New().String() + "@" + safeMessageDomain(s.MessageDomain),
			EnvelopeFrom: profile.FromEmail}
		if profile.ReplyTo != nil {
			message.ReplyTo = *profile.ReplyTo
		}
		if _, err = deliverer.Send(ctx, message); err != nil {
			return fmt.Errorf("mailing: campaign test send: %w", errs.ErrUpstream)
		}
	}
	return nil
}

func loadCampaign(ctx context.Context, q *dbgen.Queries, businessID, root, campaignID uuid.UUID) (dbgen.Campaign, error) {
	row, err := q.GetCampaign(ctx, dbgen.GetCampaignParams{ID: campaignID, TenantRootID: root})
	if err != nil {
		return dbgen.Campaign{}, err
	}
	if row.BusinessID != businessID {
		return dbgen.Campaign{}, pgx.ErrNoRows
	}
	return row, nil
}

func toCampaign(r dbgen.Campaign) Campaign {
	return Campaign{ID: r.ID, BusinessID: r.BusinessID, TenantRootID: r.TenantRootID,
		ListID: r.ListID, ProfileID: uuidPtr(r.ProfileID), Name: r.Name, Subject: r.Subject,
		Preheader: r.Preheader, BodyMarkdown: r.BodyMarkdown, TagFilter: r.TagFilter,
		TrackOpens: r.TrackOpens, TrackClicks: r.TrackClicks, Status: string(r.Status),
		ScheduledAt: timePtr(r.ScheduledAt), StartedAt: timePtr(r.StartedAt),
		CompletedAt: timePtr(r.CompletedAt), RecipientCount: r.RecipientCount,
		SentCount: r.SentCount, DeliveredCount: r.DeliveredCount, BouncedCount: r.BouncedCount,
		ComplainedCount: r.ComplainedCount, OpenedCount: r.OpenedCount, ClickedCount: r.ClickedCount,
		UnsubscribedCount: r.UnsubscribedCount, FailedCount: r.FailedCount, LastError: r.LastError,
		CreatedBy: uuidPtr(r.CreatedBy), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func toDelivery(r dbgen.MailingDelivery) Delivery {
	return Delivery{ID: r.ID, CampaignID: uuidPtr(r.CampaignID), SubscriberID: r.SubscriberID,
		Email: r.Email, Status: string(r.Status), Attempts: r.Attempts, NotBefore: r.NotBefore,
		LeaseUntil: timePtr(r.LeaseUntil), MessageID: r.MessageID,
		ProviderMessageID: r.ProviderMessageID, OpenedAt: timePtr(r.OpenedAt),
		FirstClickedAt: timePtr(r.FirstClickedAt), LastError: r.LastError, CreatedAt: r.CreatedAt}
}

func validDeliveryStatus(v string) bool {
	switch v {
	case "queued", "sending", "sent", "delivered", "bounced", "complained", "failed", "suppressed", "cancelled":
		return true
	default:
		return false
	}
}

func safeMessageDomain(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if !messageDomainPattern.MatchString(v) {
		return "mailing.localhost"
	}
	return v
}
