// Package mailing owns tenant mailing lists, subscriber consent, templates, sending
// profiles, broadcast campaigns, delivery, and tracking for Spec 013.
package mailing

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	mailrender "github.com/manyforge/manyforge/internal/mailing/render"
	mailtoken "github.com/manyforge/manyforge/internal/mailing/token"
	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/ratelimit"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

const (
	defaultPageSize = 50
	maxPageSize     = 100
)

// Service is the authenticated core mailing surface. Sealer protects list S2S secrets;
// Vault protects provider credential bundles. Relay-only profiles require neither.
type Service struct {
	DB        *db.DB
	Vault     *secrets.Vault
	Sealer    *crypto.Sealer
	Tokens    *mailtoken.Codec
	Providers interface {
		Resolve(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error)
	}
	Renderer        *mailrender.Renderer
	OutboundLimiter ratelimit.Limiter
	MessageDomain   string
	Logger          *slog.Logger
	PublicBaseURL   string
	Now             func() time.Time
	Rand            io.Reader
}

type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type List struct {
	ID           uuid.UUID `json:"id"`
	BusinessID   uuid.UUID `json:"business_id"`
	TenantRootID uuid.UUID `json:"tenant_root_id"`
	Slug         string    `json:"slug"`
	Name         string    `json:"name"`
	Description  *string   `json:"description"`
	DoubleOptIn  bool      `json:"double_opt_in"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type ListInput struct {
	Slug        string
	Name        string
	Description *string
	DoubleOptIn bool
}

type ListUpdate struct {
	Name           *string
	Description    *string
	SetDescription bool
	DoubleOptIn    *bool
}

type Subscriber struct {
	ID                uuid.UUID      `json:"id"`
	BusinessID        uuid.UUID      `json:"business_id"`
	TenantRootID      uuid.UUID      `json:"tenant_root_id"`
	ListID            uuid.UUID      `json:"list_id"`
	Email             string         `json:"email"`
	FirstName         *string        `json:"first_name"`
	LastName          *string        `json:"last_name"`
	Attributes        map[string]any `json:"attributes"`
	Status            string         `json:"status"`
	ContactID         *uuid.UUID     `json:"contact_id"`
	ConsentSource     string         `json:"consent_source"`
	ConsentAttestedBy *uuid.UUID     `json:"consent_attested_by"`
	ConsentAt         time.Time      `json:"consent_at"`
	ConfirmedAt       *time.Time     `json:"confirmed_at"`
	UnsubscribedAt    *time.Time     `json:"unsubscribed_at"`
	StatusReason      *string        `json:"status_reason"`
	Tags              []string       `json:"tags"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

type SubscriberInput struct {
	Email            string
	FirstName        *string
	LastName         *string
	Attributes       map[string]any
	Tags             []string
	SkipConfirmation bool
	ContactID        *uuid.UUID
	ConsentSource    string
}

type SubscriberUpdate struct {
	FirstName       *string
	SetFirstName    bool
	LastName        *string
	SetLastName     bool
	Attributes      map[string]any
	SetAttributes   bool
	Status          *string
	StatusReason    *string
	SetStatusReason bool
	Tags            *[]string
}

type SubscriberFilter struct {
	Query  string
	Status string
	Tag    string
	Cursor string
	Limit  int
}

type ListKey struct {
	ID             uuid.UUID  `json:"id"`
	BusinessID     uuid.UUID  `json:"business_id"`
	TenantRootID   uuid.UUID  `json:"tenant_root_id"`
	ListID         uuid.UUID  `json:"list_id"`
	PublishableKey string     `json:"publishable_key"`
	Label          *string    `json:"label"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	RevokedAt      *time.Time `json:"revoked_at"`
	HasSecret      bool       `json:"has_secret"`
	Secret         string     `json:"secret,omitempty"`
}

type SendingProfile struct {
	ID                  uuid.UUID  `json:"id"`
	BusinessID          uuid.UUID  `json:"business_id"`
	TenantRootID        uuid.UUID  `json:"tenant_root_id"`
	Mode                string     `json:"mode"`
	FromEmail           string     `json:"from_email"`
	FromName            string     `json:"from_name"`
	ReplyTo             *string    `json:"reply_to"`
	PostalAddress       *string    `json:"postal_address"`
	EmailDomainID       *uuid.UUID `json:"email_domain_id"`
	SESRegion           *string    `json:"ses_region"`
	SESConfigurationSet *string    `json:"ses_configuration_set"`
	SNSTopicARN         *string    `json:"sns_topic_arn"`
	Status              string     `json:"status"`
	LastVerifiedAt      *time.Time `json:"last_verified_at"`
	VerifyError         *string    `json:"verify_error"`
	HasCredentials      bool       `json:"has_credentials"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type ResendCredentials struct {
	APIKey        string `json:"api_key"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
}

type SESCredentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

type SendingProfileInput struct {
	Mode                string
	FromEmail           string
	FromName            string
	ReplyTo             *string
	PostalAddress       *string
	EmailDomainID       *uuid.UUID
	Resend              *ResendCredentials
	SES                 *SESCredentials
	SESRegion           *string
	SESConfigurationSet *string
	SNSTopicARN         *string
}

type Template struct {
	ID           uuid.UUID `json:"id"`
	BusinessID   uuid.UUID `json:"business_id"`
	TenantRootID uuid.UUID `json:"tenant_root_id"`
	Name         string    `json:"name"`
	Subject      string    `json:"subject"`
	Preheader    *string   `json:"preheader"`
	BodyMarkdown string    `json:"body_markdown"`
	TrackOpens   bool      `json:"track_opens"`
	TrackClicks  bool      `json:"track_clicks"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type TemplateInput struct {
	Name         string
	Subject      string
	Preheader    *string
	BodyMarkdown string
	TrackOpens   bool
	TrackClicks  bool
}

type TemplateUpdate struct {
	Name         *string
	Subject      *string
	Preheader    *string
	SetPreheader bool
	BodyMarkdown *string
	TrackOpens   *bool
	TrackClicks  *bool
}

type Suppression struct {
	ID           uuid.UUID `json:"id"`
	BusinessID   uuid.UUID `json:"business_id"`
	TenantRootID uuid.UUID `json:"tenant_root_id"`
	Email        string    `json:"email"`
	Reason       string    `json:"reason"`
	Source       string    `json:"source"`
	CreatedAt    time.Time `json:"created_at"`
}

type Campaign struct {
	ID                uuid.UUID  `json:"id"`
	BusinessID        uuid.UUID  `json:"business_id"`
	TenantRootID      uuid.UUID  `json:"tenant_root_id"`
	ListID            uuid.UUID  `json:"list_id"`
	ProfileID         *uuid.UUID `json:"profile_id"`
	Name              string     `json:"name"`
	Subject           string     `json:"subject"`
	Preheader         *string    `json:"preheader"`
	BodyMarkdown      string     `json:"body_markdown"`
	TagFilter         []string   `json:"tag_filter"`
	TrackOpens        bool       `json:"track_opens"`
	TrackClicks       bool       `json:"track_clicks"`
	Status            string     `json:"status"`
	ScheduledAt       *time.Time `json:"scheduled_at"`
	StartedAt         *time.Time `json:"started_at"`
	CompletedAt       *time.Time `json:"completed_at"`
	RecipientCount    int32      `json:"recipient_count"`
	SentCount         int32      `json:"sent_count"`
	DeliveredCount    int32      `json:"delivered_count"`
	BouncedCount      int32      `json:"bounced_count"`
	ComplainedCount   int32      `json:"complained_count"`
	OpenedCount       int32      `json:"opened_count"`
	ClickedCount      int32      `json:"clicked_count"`
	UnsubscribedCount int32      `json:"unsubscribed_count"`
	FailedCount       int32      `json:"failed_count"`
	LastError         *string    `json:"last_error"`
	CreatedBy         *uuid.UUID `json:"created_by"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type CampaignInput struct {
	ListID       uuid.UUID
	Name         string
	Subject      string
	Preheader    *string
	BodyMarkdown string
	TagFilter    []string
	TrackOpens   bool
	TrackClicks  bool
}

type CampaignUpdate struct {
	ListID       *uuid.UUID
	Name         *string
	Subject      *string
	Preheader    *string
	SetPreheader bool
	BodyMarkdown *string
	TagFilter    *[]string
	TrackOpens   *bool
	TrackClicks  *bool
}

type Delivery struct {
	ID                uuid.UUID  `json:"id"`
	CampaignID        *uuid.UUID `json:"campaign_id"`
	SubscriberID      uuid.UUID  `json:"subscriber_id"`
	Email             string     `json:"email"`
	Status            string     `json:"status"`
	Attempts          int32      `json:"attempts"`
	NotBefore         time.Time  `json:"not_before"`
	LeaseUntil        *time.Time `json:"lease_until"`
	MessageID         string     `json:"message_id"`
	ProviderMessageID *string    `json:"provider_message_id"`
	OpenedAt          *time.Time `json:"opened_at"`
	FirstClickedAt    *time.Time `json:"first_clicked_at"`
	LastError         *string    `json:"last_error"`
	CreatedAt         time.Time  `json:"created_at"`
}

type CampaignLinkStat struct {
	URL              string `json:"url"`
	ClickCount       int64  `json:"click_count"`
	UniqueClickCount int64  `json:"unique_click_count"`
}

type CampaignStats struct {
	Campaign Campaign           `json:"campaign"`
	Links    []CampaignLinkStat `json:"links"`
}

func resolveTenantRoot(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID) (uuid.UUID, error) {
	b, err := q.GetBusiness(ctx, businessID)
	if err != nil {
		return uuid.Nil, err
	}
	return b.TenantRootID, nil
}

func mapErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errs.ErrNotFound):
		return fmt.Errorf("mailing: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("mailing: duplicate: %w", errs.ErrConflict)
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return fmt.Errorf("mailing: foreign key: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23514":
		return fmt.Errorf("mailing: invalid value: %w", errs.ErrValidation)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrConflict),
		errors.Is(err, errs.ErrRateLimited), errors.Is(err, errs.ErrUpstream):
		return err
	default:
		return fmt.Errorf("mailing: query: %w", err)
	}
}

func validation(msg string) error { return fmt.Errorf("mailing: %s: %w", msg, errs.ErrValidation) }

func clampLimit(n int) int {
	if n <= 0 {
		return defaultPageSize
	}
	if n > maxPageSize {
		return maxPageSize
	}
	return n
}

var slugBad = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(v string) string {
	v = slugBad.ReplaceAllString(strings.ToLower(strings.TrimSpace(v)), "-")
	return strings.Trim(v, "-")
}

func normalizeEmail(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	a, err := mail.ParseAddress(v)
	if err != nil || a.Address != v || len(v) > 320 {
		return "", validation("invalid email")
	}
	return v, nil
}

func normalizeTags(tags []string) ([]string, error) {
	if len(tags) > 50 {
		return nil, validation("at most 50 tags are allowed")
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if tag == "" || len(tag) > 64 {
			return nil, validation("tags must be 1 to 64 characters")
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out, nil
}

func validSubscriberStatus(v string) bool {
	switch v {
	case "pending", "active", "unsubscribed", "bounced", "complained":
		return true
	default:
		return false
	}
}

func jsonObject(v map[string]any) ([]byte, error) {
	if v == nil {
		return []byte(`{}`), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, validation("attributes must be a JSON object")
	}
	if len(b) > 64*1024 {
		return nil, validation("attributes exceed 64 KiB")
	}
	return b, nil
}

func pgUUIDPtr(v *uuid.UUID) pgtype.UUID {
	if v == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *v, Valid: true}
}

func uuidPtr(v pgtype.UUID) *uuid.UUID {
	if !v.Valid {
		return nil
	}
	u := uuid.UUID(v.Bytes)
	return &u
}

func timePtr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

func strPtr(v string) *string { return &v }

func auditMutation(ctx context.Context, tx pgx.Tx, principalID, businessID, tenantRootID uuid.UUID, action, targetType string, targetID uuid.UUID, value any) error {
	return audit.Write(ctx, tx, audit.Entry{
		BusinessID:       &businessID,
		TenantRootID:     &tenantRootID,
		ActorPrincipalID: &principalID,
		Action:           action,
		TargetType:       &targetType,
		TargetID:         &targetID,
		NewValue:         value,
	})
}

func encodeCursor(kind, key string, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + "|" + key + "|" + id.String()))
}

func decodeCursor(kind, token string) (string, uuid.UUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", uuid.Nil, validation("invalid cursor")
	}
	p := strings.SplitN(string(b), "|", 3)
	if len(p) != 3 || p[0] != kind {
		return "", uuid.Nil, validation("invalid cursor")
	}
	id, err := uuid.Parse(p[2])
	if err != nil {
		return "", uuid.Nil, validation("invalid cursor")
	}
	return p[1], id, nil
}

func encodeTimeCursor(kind string, at time.Time, id uuid.UUID) string {
	return encodeCursor(kind, at.UTC().Format(time.RFC3339Nano), id)
}

func decodeTimeCursor(kind, token string) (time.Time, uuid.UUID, error) {
	key, id, err := decodeCursor(kind, token)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	at, err := time.Parse(time.RFC3339Nano, key)
	if err != nil {
		return time.Time{}, uuid.Nil, validation("invalid cursor")
	}
	return at, id, nil
}

func toList(r dbgen.MailingList) List {
	return List{ID: r.ID, BusinessID: r.BusinessID, TenantRootID: r.TenantRootID, Slug: r.Slug,
		Name: r.Name, Description: r.Description, DoubleOptIn: r.DoubleOptIn, Status: r.Status,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func toSubscriber(r dbgen.ListSubscriber, tags []string) Subscriber {
	attributes := map[string]any{}
	_ = json.Unmarshal(r.Attributes, &attributes)
	return Subscriber{
		ID: r.ID, BusinessID: r.BusinessID, TenantRootID: r.TenantRootID, ListID: r.ListID,
		Email: r.Email, FirstName: r.FirstName, LastName: r.LastName, Attributes: attributes,
		Status: string(r.Status), ContactID: uuidPtr(r.ContactID), ConsentSource: string(r.ConsentSource),
		ConsentAttestedBy: uuidPtr(r.ConsentAttestedBy), ConsentAt: r.ConsentAt,
		ConfirmedAt: timePtr(r.ConfirmedAt), UnsubscribedAt: timePtr(r.UnsubscribedAt),
		StatusReason: r.StatusReason, Tags: tags, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
