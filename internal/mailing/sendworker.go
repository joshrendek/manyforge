package mailing

import (
	"container/list"
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

const (
	fanoutGlobalBudget      = 1000
	fanoutPerCampaignBudget = 250
	rollupCampaignBudget    = 100
	rollupLeaseSeconds      = 60
	compiledCacheMaxEntries = 128
	compiledCacheMaxBytes   = 8 << 20
	compiledCacheTTL        = 15 * time.Minute
	providerWebhookPruneBudget = 256
)

// SendWorker drains scheduled campaigns and the shared mailing delivery queue. Claims and
// writebacks use short transactions. In particular, no transaction is held while rendering,
// waiting for the rate limiter, resolving a provider, or performing network I/O. Delivery is
// at-least-once: a crash after provider acceptance but before completion causes a retry after
// Lease; providers receive the stable delivery ID as their idempotency key where supported.
type SendWorker struct {
	Service           *Service
	Batch             int
	Lease             time.Duration
	Every             time.Duration
	FanoutGlobal      int
	FanoutPerCampaign int
	RollupBatch       int
	Limiter           interface{ Allow(string) bool }
	Logger            *slog.Logger
	Now               func() time.Time

	compiledMu sync.Mutex
	compiled   *compiledContentCache
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

type compiledCacheKey struct {
	contentKind                    string
	contentID, profileID           uuid.UUID
	contentVersion, profileVersion int64
}

type compiledCacheEntry struct {
	key       compiledCacheKey
	value     mailrender.Compiled
	bytes     int
	expiresAt time.Time
}

type compiledContentCache struct {
	mu                   sync.Mutex
	entries              map[compiledCacheKey]*list.Element
	lru                  *list.List
	bytes                int
	maxEntries, maxBytes int
	ttl                  time.Duration
	now                  func() time.Time
}

func newCompiledContentCache(maxEntries, maxBytes int, ttl time.Duration, now func() time.Time) *compiledContentCache {
	return &compiledContentCache{
		entries: make(map[compiledCacheKey]*list.Element), lru: list.New(),
		maxEntries: maxEntries, maxBytes: maxBytes, ttl: ttl, now: now,
	}
}

func (c *compiledContentCache) get(key compiledCacheKey) (mailrender.Compiled, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return mailrender.Compiled{}, false
	}
	entry := element.Value.(*compiledCacheEntry)
	if !c.now().Before(entry.expiresAt) {
		c.remove(element)
		return mailrender.Compiled{}, false
	}
	c.lru.MoveToFront(element)
	return entry.value, true
}

func (c *compiledContentCache) put(key compiledCacheKey, value mailrender.Compiled) {
	size := len(value.HTML)
	if size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictExpired(c.now())
	if existing := c.entries[key]; existing != nil {
		c.remove(existing)
	}
	for existingKey, element := range c.entries {
		if (existingKey.contentKind == key.contentKind && existingKey.contentID == key.contentID &&
			existingKey.contentVersion != key.contentVersion) ||
			(existingKey.profileID == key.profileID && existingKey.profileVersion != key.profileVersion) {
			c.remove(element)
		}
	}
	entry := &compiledCacheEntry{
		key: key, value: value, bytes: size, expiresAt: c.now().Add(c.ttl),
	}
	c.entries[key] = c.lru.PushFront(entry)
	c.bytes += size
	for len(c.entries) > c.maxEntries || c.bytes > c.maxBytes {
		c.remove(c.lru.Back())
	}
}

func (c *compiledContentCache) invalidateContent(contentID uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, element := range c.entries {
		if key.contentID == contentID {
			c.remove(element)
		}
	}
}

func (c *compiledContentCache) stats() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictExpired(c.now())
	return len(c.entries), c.bytes
}

func (c *compiledContentCache) evictExpired(now time.Time) {
	for element := c.lru.Back(); element != nil; {
		previous := element.Prev()
		if !now.Before(element.Value.(*compiledCacheEntry).expiresAt) {
			c.remove(element)
		}
		element = previous
	}
}

func (c *compiledContentCache) remove(element *list.Element) {
	if element == nil {
		return
	}
	entry := element.Value.(*compiledCacheEntry)
	delete(c.entries, entry.key)
	c.bytes -= entry.bytes
	c.lru.Remove(element)
}

func (w *SendWorker) compiledCache() *compiledContentCache {
	w.compiledMu.Lock()
	defer w.compiledMu.Unlock()
	if w.compiled == nil {
		w.compiled = newCompiledContentCache(
			compiledCacheMaxEntries, compiledCacheMaxBytes, compiledCacheTTL, w.now,
		)
	}
	return w.compiled
}

// InvalidateCompiledContent removes every cached immutable version for contentID.
func (w *SendWorker) InvalidateCompiledContent(contentID uuid.UUID) {
	w.compiledCache().invalidateContent(contentID)
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
	if err := w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var pruned int
		return tx.QueryRow(ctx,
			"SELECT mailing_prune_expired_provider_webhooks($1)",
			providerWebhookPruneBudget,
		).Scan(&pruned)
	}); err != nil {
		return fmt.Errorf("prune expired provider webhooks: %w", err)
	}
	globalBudget := w.FanoutGlobal
	if globalBudget <= 0 {
		globalBudget = fanoutGlobalBudget
	}
	perCampaignBudget := w.FanoutPerCampaign
	if perCampaignBudget <= 0 {
		perCampaignBudget = fanoutPerCampaignBudget
	}
	if perCampaignBudget > globalBudget {
		perCampaignBudget = globalBudget
	}
	campaignLimit := (globalBudget + perCampaignBudget - 1) / perCampaignBudget
	campaignIDs, err := w.claimCampaigns(ctx, campaignLimit)
	if err != nil {
		return err
	}
	remaining := globalBudget
	for _, campaignID := range campaignIDs {
		batch := perCampaignBudget
		if batch > remaining {
			batch = remaining
		}
		if _, err := w.fanout(ctx, campaignID, batch); err != nil {
			return fmt.Errorf("fan out campaign %s: %w", campaignID, err)
		}
		remaining -= batch
		if remaining == 0 {
			break
		}
	}
	deliveries, err := w.claimDeliveries(ctx)
	if err != nil {
		return err
	}
	for i := range deliveries {
		w.deliver(ctx, deliveries[i])
	}
	return w.rollupChangedCampaigns(ctx)
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

func (w *SendWorker) fanout(ctx context.Context, campaignID uuid.UUID, batch int) (bool, error) {
	done := false
	err := w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var inserted int
		return tx.QueryRow(ctx,
			"SELECT inserted_count, fanout_done FROM mailing_fanout_batch($1,$2,$3)",
			campaignID, batch, safeMessageDomain(w.Service.MessageDomain),
		).Scan(&inserted, &done)
	})
	return done, err
}

func (w *SendWorker) rollupChangedCampaigns(ctx context.Context) error {
	limit := w.RollupBatch
	if limit <= 0 || limit > rollupCampaignBudget {
		limit = rollupCampaignBudget
	}
	token := uuid.New()
	var campaignIDs []uuid.UUID
	if err := w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT mailing_claim_changed_campaign_rollups($1,$2,$3)",
			token, limit, rollupLeaseSeconds,
		).Scan(&campaignIDs)
	}); err != nil {
		return fmt.Errorf("claim changed campaign rollups: %w", err)
	}
	processed := make([]uuid.UUID, 0, len(campaignIDs))
	var rollupErr error
	for _, campaignID := range campaignIDs {
		var ok bool
		err := w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT mailing_rollup_changed_campaign($1)", campaignID).Scan(&ok)
		})
		if err != nil {
			rollupErr = fmt.Errorf("roll up campaign %s: %w", campaignID, err)
			break
		}
		if ok {
			processed = append(processed, campaignID)
		}
	}
	if len(processed) > 0 {
		if err := w.Service.DB.WithTx(ctx, func(tx pgx.Tx) error {
			var completed int
			return tx.QueryRow(ctx,
				"SELECT mailing_complete_changed_campaign_rollups($1,$2)",
				token, processed,
			).Scan(&completed)
		}); err != nil {
			return fmt.Errorf("complete changed campaign rollups: %w", err)
		}
	}
	return rollupErr
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
	key := compiledCacheKey{
		contentKind: contentKind, contentID: contentID,
		contentVersion: d.ContentUpdatedAt.UnixNano(),
		profileID:      p.provider.ID, profileVersion: p.provider.UpdatedAt.UnixNano(),
	}
	cache := w.compiledCache()
	if cached, ok := cache.get(key); ok {
		return cached, nil
	}
	compiled, err := w.Service.Renderer.Compile(mailrender.Input{BodyMarkdown: d.BodyMarkdown,
		FromName: p.fromName, Preheader: stringValue(d.Preheader),
		PostalAddress: stringValue(p.postalAddress)})
	if err == nil {
		cache.put(key, compiled)
	}
	return compiled, err
}

func (w *SendWorker) resolveProfile(ctx context.Context, profileID uuid.UUID) (workerProfile, error) {
	return w.Service.resolveSystemProfile(ctx, profileID, false)
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
