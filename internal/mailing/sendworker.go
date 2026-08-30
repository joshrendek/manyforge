package mailing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	mailrender "github.com/manyforge/manyforge/internal/mailing/render"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

const fanoutBatchSize = 1000

// SendWorker drains scheduled campaigns and the shared mailing delivery queue. Claims and
// writebacks use short transactions. In particular, no transaction is held while rendering,
// waiting for the rate limiter, resolving a provider, or performing network I/O. Delivery is
// at-least-once: a crash after provider acceptance but before completion causes a retry after
// Lease; providers receive the stable delivery ID as their idempotency key where supported.
type SendWorker struct {
	Service *Service
	Batch   int
	Lease   time.Duration
	Every   time.Duration
	Limiter interface{ Allow(string) bool }
	Logger  *slog.Logger
	Now     func() time.Time

	compiled sync.Map // immutable campaign or versioned template/profile key -> mailrender.Compiled
}

type claimedDelivery struct {
	ID, BusinessID, TenantRootID, SourceID, SubscriberID uuid.UUID
	CampaignID, TemplateID                               *uuid.UUID
	ContentUpdatedAt                                     time.Time
	Email, MessageID, Subject, BodyMarkdown, ListName    string
	Preheader, FirstName, LastName                       *string
	Attempts, ClaimGeneration                            int
	TrackOpens, TrackClicks                              bool
	ProfileID                                            uuid.UUID
}

type workerProfile struct {
	provider               mailprovider.Profile
	fromName               string
	replyTo, postalAddress *string
}

func (w *SendWorker) Run(ctx context.Context) {
	if w == nil || w.Service == nil || w.Service.DB == nil {
		return
	}
	every := w.Every
	if every <= 0 {
		every = 2 * time.Second
	}
	if err := w.Tick(ctx); err != nil {
		w.logger().ErrorContext(ctx, "mailing worker tick failed", "err", err)
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.Tick(ctx); err != nil {
				w.logger().ErrorContext(ctx, "mailing worker tick failed", "err", err)
			}
		}
	}
}

// Tick performs one bounded claim/send/rollup cycle. It is exported for deterministic tests.
func (w *SendWorker) Tick(ctx context.Context) error {
	if w == nil || w.Service == nil || w.Service.DB == nil {
		return errors.New("mailing worker is not configured")
	}
	campaignIDs, err := w.claimCampaigns(ctx, 10)
	if err != nil {
		return err
	}
	for _, campaignID := range campaignIDs {
		for {
			done, err := w.fanout(ctx, campaignID)
			if err != nil {
				return fmt.Errorf("fan out campaign %s: %w", campaignID, err)
			}
			if done {
				break
			}
		}
	}
	deliveries, err := w.claimDeliveries(ctx)
	if err != nil {
		return err
	}
	for i := range deliveries {
		w.deliver(ctx, deliveries[i])
	}
	return w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT mailing_rollup_campaigns()")
		return err
	})
}

func (w *SendWorker) claimCampaigns(ctx context.Context, limit int) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT campaign_id FROM mailing_claim_campaigns_for_fanout($1)", limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

func (w *SendWorker) fanout(ctx context.Context, campaignID uuid.UUID) (bool, error) {
	done := false
	err := w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var inserted int
		return tx.QueryRow(ctx,
			"SELECT inserted_count, fanout_done FROM mailing_fanout_batch($1,$2,$3)",
			campaignID, fanoutBatchSize, safeMessageDomain(w.Service.MessageDomain),
		).Scan(&inserted, &done)
	})
	return done, err
}

func (w *SendWorker) claimDeliveries(ctx context.Context) ([]claimedDelivery, error) {
	batch := w.Batch
	if batch <= 0 {
		batch = 100
	}
	lease := w.Lease
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	var out []claimedDelivery
	err := w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT delivery_id, business_id, tenant_root_id,
			source_id, campaign_id, template_id, content_updated_at,
			subscriber_id, email, attempts, claim_generation, message_id, subject, preheader,
			body_markdown, track_opens, track_clicks, list_name, first_name, last_name,
			profile_id
			FROM mailing_claim_deliveries($1,$2)`, batch, lease)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d claimedDelivery
			var campaignID, templateID pgtype.UUID
			if err := rows.Scan(&d.ID, &d.BusinessID, &d.TenantRootID, &d.SourceID,
				&campaignID, &templateID, &d.ContentUpdatedAt,
				&d.SubscriberID, &d.Email, &d.Attempts, &d.ClaimGeneration, &d.MessageID, &d.Subject,
				&d.Preheader, &d.BodyMarkdown, &d.TrackOpens, &d.TrackClicks,
				&d.ListName, &d.FirstName, &d.LastName, &d.ProfileID); err != nil {
				return err
			}
			if campaignID.Valid {
				id := uuid.UUID(campaignID.Bytes)
				d.CampaignID = &id
			}
			if templateID.Valid {
				id := uuid.UUID(templateID.Bytes)
				d.TemplateID = &id
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

func (w *SendWorker) deliver(ctx context.Context, d claimedDelivery) {
	now := w.now()
	limiter := w.Limiter
	if limiter == nil {
		limiter = w.Service.OutboundLimiter
	}
	if limiter != nil && !limiter.Allow("mailing:biz:"+d.BusinessID.String()) {
		if err := w.release(ctx, d.ID, d.ClaimGeneration, now.Add(time.Second)); err != nil {
			w.logger().ErrorContext(ctx, "mailing delivery release failed", "delivery_id", d.ID, "err", err)
		}
		return
	}
	profile, err := w.resolveProfile(ctx, d.ProfileID)
	if err != nil {
		w.fail(ctx, d, err)
		return
	}
	if w.Service.Providers == nil || w.Service.Renderer == nil || w.Service.Tokens == nil {
		w.fail(ctx, d, errors.New("mailing delivery dependencies are unavailable"))
		return
	}
	deliverer, err := w.Service.Providers.Resolve(ctx, profile.provider)
	if err != nil {
		w.fail(ctx, d, err)
		return
	}
	compiled, err := w.compile(d, profile)
	if err != nil {
		w.fail(ctx, d, err)
		return
	}
	base := strings.TrimRight(w.Service.PublicBaseURL, "/")
	campaignID := uuid.Nil
	if d.CampaignID != nil {
		campaignID = *d.CampaignID
	}
	unsubToken := w.Service.Tokens.EncodeUnsubscribe(d.SubscriberID, campaignID)
	unsubURL := base + "/m/u/" + url.PathEscape(unsubToken)
	tracking := mailrender.Tracking{}
	if d.TrackOpens {
		tracking.OpenURL = base + "/m/o/" + url.PathEscape(w.Service.Tokens.EncodeOpen(d.ID))
	}
	if d.TrackClicks {
		tracking.ClickURL = func(destination string) (string, error) {
			token, err := w.Service.Tokens.EncodeClick(d.ID, destination)
			if err != nil {
				return "", err
			}
			return base + "/m/c/" + url.PathEscape(token), nil
		}
	}
	rendered, err := w.Service.Renderer.Render(compiled, mailrender.Variables{
		FirstName: stringValue(d.FirstName), LastName: stringValue(d.LastName),
		Email: d.Email, UnsubscribeURL: unsubURL, ListName: d.ListName,
	}, tracking)
	if err != nil {
		w.fail(ctx, d, err)
		return
	}
	message := notify.Mail{
		From: (&mail.Address{Name: profile.fromName, Address: profile.provider.FromEmail}).String(),
		To:   d.Email, Subject: d.Subject, BodyText: rendered.Text, BodyHTML: rendered.HTML,
		MessageID: d.MessageID, EnvelopeFrom: profile.provider.FromEmail,
		ExtraHeaders: map[string]string{
			"List-Unsubscribe":      "<" + unsubURL + ">",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
			"List-Id":               d.ListName + " <" + d.SourceID.String() + "." + safeMessageDomain(w.Service.MessageDomain) + ">",
			"Precedence":            "bulk",
			"X-MF-Delivery":         d.ID.String(),
		},
	}
	if profile.replyTo != nil {
		message.ReplyTo = *profile.replyTo
	}
	renewed, err := w.renew(ctx, d.ID, d.ClaimGeneration)
	if err != nil {
		w.logger().ErrorContext(ctx, "mailing delivery lease renewal failed", "delivery_id", d.ID, "err", err)
		return
	}
	if !renewed {
		w.logger().WarnContext(ctx, "mailing delivery lease was lost before send", "delivery_id", d.ID)
		return
	}
	result, err := deliverer.Send(ctx, message) // no database transaction is open here
	if err != nil {
		w.fail(ctx, d, err)
		return
	}
	if err := w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var changed bool
		return tx.QueryRow(ctx, "SELECT mailing_complete_delivery($1,$2,$3)", d.ID, d.ClaimGeneration, result.ProviderID).Scan(&changed)
	}); err != nil {
		w.logger().ErrorContext(ctx, "mailing delivery completion failed", "delivery_id", d.ID, "err", err)
	}
}

func (w *SendWorker) compile(d claimedDelivery, p workerProfile) (mailrender.Compiled, error) {
	contentID := d.SourceID
	contentKind := "campaign"
	if d.TemplateID != nil {
		contentID = *d.TemplateID
		contentKind = "template"
	}
	key := fmt.Sprintf("%s:%s:%d:profile:%s:%d", contentKind, contentID,
		d.ContentUpdatedAt.UnixNano(), p.provider.ID, p.provider.UpdatedAt.UnixNano())
	if cached, ok := w.compiled.Load(key); ok {
		return cached.(mailrender.Compiled), nil
	}
	compiled, err := w.Service.Renderer.Compile(mailrender.Input{BodyMarkdown: d.BodyMarkdown,
		FromName: p.fromName, Preheader: stringValue(d.Preheader),
		PostalAddress: stringValue(p.postalAddress)})
	if err == nil {
		w.compiled.Store(key, compiled)
	}
	return compiled, err
}

func (w *SendWorker) resolveProfile(ctx context.Context, profileID uuid.UUID) (workerProfile, error) {
	return w.Service.resolveSystemProfile(ctx, profileID, false)
}

func (s *Service) resolveBusinessProfile(ctx context.Context, businessID uuid.UUID) (workerProfile, error) {
	return s.resolveSystemProfile(ctx, businessID, true)
}

func (s *Service) resolveSystemProfile(ctx context.Context, id uuid.UUID, byBusiness bool) (workerProfile, error) {
	var p workerProfile
	var mode, fromEmail string
	var updated time.Time
	var emailDomainID, secretRef *uuid.UUID
	var sealed, sesRegion, sesConfig *string
	function := "mailing_profile_context"
	if byBusiness {
		function = "mailing_business_profile_context"
	}
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT profile_id, updated_at, mode::text, from_email::text,
			from_name, reply_to::text, postal_address, email_domain_id, secret_ref,
			credential_sealed, ses_region, ses_configuration_set
			FROM `+function+`($1)`, id).Scan(
			&p.provider.ID, &updated, &mode, &fromEmail, &p.fromName, &p.replyTo,
			&p.postalAddress, &emailDomainID, &secretRef, &sealed, &sesRegion, &sesConfig)
	})
	if err != nil {
		return workerProfile{}, fmt.Errorf("mailing profile context: %w", err)
	}
	p.provider.UpdatedAt, p.provider.Mode, p.provider.FromEmail = updated, mode, fromEmail
	p.provider.EmailDomainID = emailDomainID
	p.provider.SESRegion, p.provider.SESConfigurationSet = stringValue(sesRegion), stringValue(sesConfig)
	if secretRef != nil {
		if sealed == nil || s.Sealer == nil {
			return workerProfile{}, errors.New("mailing profile credential is unavailable")
		}
		credential, err := s.Sealer.Open(*sealed)
		if err != nil {
			return workerProfile{}, errors.New("mailing profile credential could not be opened")
		}
		defer clear(credential)
		switch mode {
		case "resend":
			var creds ResendCredentials
			if err := json.Unmarshal(credential, &creds); err != nil {
				return workerProfile{}, errors.New("mailing stored Resend credentials are invalid")
			}
			p.provider.ResendAPIKey = creds.APIKey
		case "ses":
			var creds SESCredentials
			if err := json.Unmarshal(credential, &creds); err != nil {
				return workerProfile{}, errors.New("mailing stored SES credentials are invalid")
			}
			p.provider.SESAccessKeyID, p.provider.SESSecretAccessKey = creds.AccessKeyID, creds.SecretAccessKey
		}
	}
	return p, nil
}

func (w *SendWorker) release(ctx context.Context, id uuid.UUID, generation int, notBefore time.Time) error {
	return w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var changed bool
		return tx.QueryRow(ctx, "SELECT mailing_release_delivery($1,$2,$3)", id, generation, notBefore).Scan(&changed)
	})
}

func (w *SendWorker) renew(ctx context.Context, id uuid.UUID, generation int) (bool, error) {
	lease := w.Lease
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	var changed bool
	err := w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT mailing_renew_delivery($1,$2,$3)", id, generation, lease).Scan(&changed)
	})
	return changed, err
}

func (w *SendWorker) fail(ctx context.Context, d claimedDelivery, sendErr error) {
	classification := mailprovider.Classify(sendErr, d.Attempts, w.now())
	err := w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var changed bool
		return tx.QueryRow(ctx, "SELECT mailing_fail_delivery($1,$2,$3,$4,$5)", d.ID,
			d.ClaimGeneration, safeProviderMessage(sendErr), classification.Status, classification.NotBefore).Scan(&changed)
	})
	if err != nil {
		w.logger().ErrorContext(ctx, "mailing delivery failure writeback failed", "delivery_id", d.ID, "err", err)
	}
}

func (w *SendWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *SendWorker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	if w.Service != nil && w.Service.Logger != nil {
		return w.Service.Logger
	}
	return slog.Default()
}
