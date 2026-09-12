package mailing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	mailrender "github.com/manyforge/manyforge/internal/mailing/render"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

// PreviewInput contains template content and optional layout overrides. BodyMarkdown
// is limited to one mebibyte of Unicode code points before rendering.
type PreviewInput struct {
	BodyMarkdown  string
	Preheader     *string
	FromName      *string
	PostalAddress *string
}

// VerifySendingProfile checks the live provider with no database transaction
// held open, then records either verified or error. Provider verification
// failures are an expected result and return the updated profile with HTTP 200.
func (s *Service) VerifySendingProfile(ctx context.Context, principalID, businessID uuid.UUID) (SendingProfile, error) {
	if err := s.authorizeSendingProfileVerification(ctx, principalID, businessID); err != nil {
		return SendingProfile{}, err
	}
	profile, providerProfile, err := s.loadProviderProfile(ctx, principalID, businessID)
	if err != nil && profile.ID == uuid.Nil {
		return SendingProfile{}, err
	}
	verifyErr := err
	var resendProvisioningToken uuid.UUID
	if profile.Mode == "resend" {
		resendProvisioningToken, err = s.claimResendProvisioning(ctx, principalID, profile)
		if err != nil {
			return SendingProfile{}, err
		}
	}
	var deliverer mailprovider.Deliverer
	if verifyErr == nil && s.Providers == nil {
		verifyErr = mailprovider.ErrProviderConfiguration
	}
	if verifyErr == nil {
		deliverer, verifyErr = s.Providers.Resolve(ctx, providerProfile)
		if verifyErr == nil {
			verifier, ok := deliverer.(mailprovider.Verifier)
			if !ok {
				verifyErr = mailprovider.ErrProviderConfiguration
			} else {
				verifyErr = verifier.Verify(ctx)
			}
		}
	}
	if verifyErr == nil && profile.Mode == "resend" {
		provisioner, ok := deliverer.(mailprovider.ResendWebhookProvisioner)
		if !ok {
			verifyErr = mailprovider.ErrResendWebhook
		} else if baseURL := strings.TrimRight(strings.TrimSpace(s.PublicBaseURL), "/"); baseURL == "" {
			verifyErr = mailprovider.ErrPublicURL
		} else {
			endpoint := baseURL + "/api/v1/inbound/mailing/" + profile.ID.String() + "/resend"
			// EnsureWebhook may delete or create remote webhooks while reconciling.
			// Commit cleanup intent before entering it, not during read-only checks.
			if err := s.markResendWebhookMutation(ctx, principalID, profile, resendProvisioningToken); err != nil {
				s.releaseResendProvisioning(ctx, principalID, profile, resendProvisioningToken)
				return SendingProfile{}, err
			}
			webhook, _, provisionErr := provisioner.EnsureWebhook(ctx, endpoint, providerProfile.ResendWebhookID)
			if provisionErr != nil {
				verifyErr = fmt.Errorf("%w: %w", mailprovider.ErrResendWebhook, provisionErr)
			} else {
				out, persistErr := s.persistResendWebhookVerification(
					ctx, principalID, businessID, profile, providerProfile.ResendAPIKey,
					webhook, resendProvisioningToken)
				if persistErr != nil {
					s.releaseResendProvisioning(ctx, principalID, profile, resendProvisioningToken)
				}
				return out, persistErr
			}
		}
	}
	if resendProvisioningToken != uuid.Nil {
		s.releaseResendProvisioning(ctx, principalID, profile, resendProvisioningToken)
	}
	status, message := "verified", ""
	if verifyErr != nil {
		status, message = "error", providerVerificationMessage(verifyErr)
	}
	feedbackStatus, feedbackMessage := profile.FeedbackStatus, ""
	switch profile.Mode {
	case "resend":
		feedbackStatus = "pending"
		if verifyErr != nil {
			feedbackStatus, feedbackMessage = "error", message
		}
	case "relay":
		if verifyErr == nil {
			feedbackStatus = "ready"
		} else {
			feedbackStatus, feedbackMessage = "error", message
		}
	case "ses":
		if feedbackStatus != "ready" {
			feedbackStatus = "pending"
		}
		if errors.Is(verifyErr, mailprovider.ErrSESFeedback) || errors.Is(verifyErr, mailprovider.ErrSESConfiguration) {
			feedbackStatus, feedbackMessage = "error", message
		}
	}

	var out SendingProfile
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		if err := requireSendingProfileVerification(ctx, tx, principalID, businessID, profile.TenantRootID); err != nil {
			return err
		}
		row, err := dbgen.New(tx).SetMailingSendingProfileVerification(ctx, dbgen.SetMailingSendingProfileVerificationParams{
			Status: status, VerifyError: message,
			FeedbackStatus: feedbackStatus, FeedbackError: feedbackMessage,
			ID: profile.ID, TenantRootID: profile.TenantRootID,
			ExpectedUpdatedAt: profile.UpdatedAt,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("mailing: profile changed during verification: %w", errs.ErrConflict)
			}
			return err
		}
		if err := auditMutation(ctx, tx, principalID, businessID, profile.TenantRootID,
			"mailing.sending_profile.verified", "mailing_sending_profile", profile.ID,
			map[string]any{"mode": profile.Mode, "status": status}); err != nil {
			return err
		}
		out = toSendingProfile(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) persistResendWebhookVerification(
	ctx context.Context,
	principalID, businessID uuid.UUID,
	profile SendingProfile,
	apiKey string,
	webhook mailprovider.ResendWebhook,
	provisioningToken uuid.UUID,
) (SendingProfile, error) {
	if s.Vault == nil {
		return SendingProfile{}, validation("mailing credential storage is not configured")
	}
	raw, err := json.Marshal(resendStoredCredentials{
		Version: 2, APIKey: apiKey, WebhookID: webhook.ID, WebhookSecret: webhook.SigningSecret,
	})
	if err != nil {
		return SendingProfile{}, errors.New("mailing: encode Resend webhook credential")
	}
	defer clear(raw)
	var out SendingProfile
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		if err := requireSendingProfileVerification(ctx, tx, principalID, businessID, profile.TenantRootID); err != nil {
			return err
		}
		q := dbgen.New(tx)
		current, err := q.GetMailingSendingProfile(ctx, dbgen.GetMailingSendingProfileParams{
			BusinessID: businessID, TenantRootID: profile.TenantRootID,
		})
		if err != nil {
			return err
		}
		newSecretID, err := s.Vault.Put(ctx, tx, businessID, "mailing", raw)
		if err != nil {
			return err
		}
		row, err := q.SetMailingResendWebhookVerification(ctx, dbgen.SetMailingResendWebhookVerificationParams{
			SecretRef: newSecretID, ID: profile.ID, TenantRootID: profile.TenantRootID,
			ExpectedUpdatedAt: profile.UpdatedAt, Token: provisioningToken,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("mailing: profile changed during Resend webhook provisioning: %w", errs.ErrConflict)
			}
			return err
		}
		if current.SecretRef.Valid {
			if err := s.Vault.Delete(ctx, tx, businessID, uuid.UUID(current.SecretRef.Bytes)); err != nil &&
				!errors.Is(err, errs.ErrNotFound) {
				return err
			}
		}
		if err := auditMutation(ctx, tx, principalID, businessID, profile.TenantRootID,
			"mailing.sending_profile.verified", "mailing_sending_profile", profile.ID,
			map[string]any{"mode": profile.Mode, "status": "verified", "feedback_status": "ready"}); err != nil {
			return err
		}
		out = toSendingProfile(row)
		return nil
	})
	if err == nil && s.Providers != nil {
		s.Providers.Invalidate(profile.ID)
	}
	return out, mapErr(err)
}
func (s *Service) claimResendProvisioning(
	ctx context.Context, principalID uuid.UUID, profile SendingProfile,
) (uuid.UUID, error) {
	token := uuid.New()
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		if err := requireSendingProfileVerification(ctx, tx, principalID, profile.BusinessID, profile.TenantRootID); err != nil {
			return err
		}
		_, err := dbgen.New(tx).ClaimMailingResendProvisioning(ctx, dbgen.ClaimMailingResendProvisioningParams{
			Token: token, RequireCleanup: false, ID: profile.ID, TenantRootID: profile.TenantRootID,
			ExpectedUpdatedAt: profile.UpdatedAt,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("mailing: Resend provisioning already in progress: %w", errs.ErrConflict)
		}
		return err
	})
	if err != nil {
		return uuid.Nil, mapErr(err)
	}
	return token, nil
}

func (s *Service) markResendWebhookMutation(
	ctx context.Context, principalID uuid.UUID, profile SendingProfile, token uuid.UUID,
) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		if err := requireSendingProfileVerification(ctx, tx, principalID, profile.BusinessID, profile.TenantRootID); err != nil {
			return err
		}
		_, err := dbgen.New(tx).MarkMailingResendWebhookMutation(ctx, dbgen.MarkMailingResendWebhookMutationParams{
			ID: profile.ID, BusinessID: profile.BusinessID, TenantRootID: profile.TenantRootID,
			ExpectedUpdatedAt: profile.UpdatedAt, Token: token,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("mailing: Resend provisioning lease lost or profile changed: %w", errs.ErrConflict)
		}
		return err
	})
	return mapErr(err)
}

func (s *Service) releaseResendProvisioning(
	ctx context.Context, principalID uuid.UUID, profile SendingProfile, token uuid.UUID,
) {
	if token == uuid.Nil {
		return
	}
	_ = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).ReleaseMailingResendProvisioning(ctx, dbgen.ReleaseMailingResendProvisioningParams{
			ID: profile.ID, TenantRootID: profile.TenantRootID, Token: token,
		})
		return err
	})
}

func (s *Service) authorizeSendingProfileVerification(
	ctx context.Context, principalID, businessID uuid.UUID,
) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		root, err := resolveTenantRoot(ctx, dbgen.New(tx), businessID)
		if err != nil {
			return err
		}
		return requireSendingProfileVerification(ctx, tx, principalID, businessID, root)
	})
	return mapErr(err)
}

func requireSendingProfileVerification(
	ctx context.Context,
	tx pgx.Tx,
	principalID, businessID, tenantRootID uuid.UUID,
) error {
	var operational bool
	if err := tx.QueryRow(ctx, `SELECT mailing_business_operational($1,$2)`, businessID, tenantRootID).Scan(&operational); err != nil {
		return err
	}
	if !operational {
		return errs.ErrNotFound
	}
	permissions, err := authz.Resolve(ctx, tx, principalID, businessID)
	if err != nil {
		return err
	}
	if !permissions.Has(authz.PermMailingSend) {
		return errs.ErrNotFound
	}
	return nil
}

// charges the business outbound limiter before contacting the provider.
func (s *Service) TestSendingProfile(ctx context.Context, principalID, businessID uuid.UUID, recipient string) error {
	to, err := normalizeEmail(recipient)
	if err != nil {
		return err
	}
	if err := s.checkTestRecipientSuppression(ctx, principalID, businessID, to); err != nil {
		return err
	}
	profile, providerProfile, err := s.loadProviderProfile(ctx, principalID, businessID)
	if err != nil {
		return err
	}
	if profile.Status != "verified" || profile.FeedbackStatus != "ready" {
		return validation("sending profile must be verified and feedback-ready")
	}
	if s.OutboundLimiter != nil && !s.OutboundLimiter.Allow("ob:biz:"+businessID.String()) {
		return fmt.Errorf("mailing: outbound rate limit: %w", errs.ErrRateLimited)
	}
	if s.Providers == nil || s.Renderer == nil {
		return errors.New("mailing: delivery is not configured")
	}
	deliverer, err := s.Providers.Resolve(ctx, providerProfile)
	if err != nil {
		return fmt.Errorf("mailing: resolve provider: %w", errs.ErrUpstream)
	}
	rendered, err := s.Renderer.RenderInput(mailrender.Input{
		BodyMarkdown: "# Your mailing profile is ready\n\nThis is a test message from ManyForge.",
		FromName:     profile.FromName, PostalAddress: stringValue(profile.PostalAddress),
	}, mailrender.Variables{Email: to, UnsubscribeURL: "#", ListName: "Test message"}, mailrender.Tracking{})
	if err != nil {
		return err
	}
	domain := strings.TrimSpace(s.MessageDomain)
	if domain == "" || strings.ContainsAny(domain, "\r\n@") {
		domain = "mailing.localhost"
	}
	message := notify.Mail{
		From: (&mail.Address{Name: profile.FromName, Address: profile.FromEmail}).String(),
		To:   to, Subject: "[TEST] ManyForge mailing profile",
		BodyText: rendered.Text, BodyHTML: rendered.HTML,
		MessageID: uuid.New().String() + "@" + domain,
	}
	if profile.ReplyTo != nil {
		message.ReplyTo = *profile.ReplyTo
	}
	if _, err := deliverer.Send(ctx, message); err != nil {
		return fmt.Errorf("mailing: provider test send: %w", errs.ErrUpstream)
	}
	return nil
}

// Preview renders sample recipient output without tracking. Profile defaults are
// used when present; explicit input values override them.
func (s *Service) Preview(ctx context.Context, principalID, businessID uuid.UUID, in PreviewInput) (mailrender.Output, error) {
	if utf8.RuneCountInString(in.BodyMarkdown) > 1<<20 {
		return mailrender.Output{}, validation("body_markdown must not exceed 1 MiB")
	}
	fromName, postalAddress := "ManyForge", ""
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		profile, err := q.GetMailingSendingProfile(ctx, dbgen.GetMailingSendingProfileParams{BusinessID: businessID, TenantRootID: root})
		if err == nil {
			fromName = profile.FromName
			postalAddress = stringValue(profile.PostalAddress)
			return nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return mailrender.Output{}, mapErr(err)
	}
	if in.FromName != nil && strings.TrimSpace(*in.FromName) != "" {
		fromName = strings.TrimSpace(*in.FromName)
	}
	if in.PostalAddress != nil {
		postalAddress = strings.TrimSpace(*in.PostalAddress)
	}
	if s.Renderer == nil {
		return mailrender.Output{}, errors.New("mailing: renderer is not configured")
	}
	return s.Renderer.RenderInput(mailrender.Input{
		BodyMarkdown: in.BodyMarkdown, FromName: fromName,
		Preheader: stringValue(in.Preheader), PostalAddress: postalAddress,
	}, mailrender.Variables{
		FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com",
		UnsubscribeURL: "#", ListName: "Sample list",
	}, mailrender.Tracking{})
}

func (s *Service) loadProviderProfile(ctx context.Context, principalID, businessID uuid.UUID) (SendingProfile, mailprovider.Profile, error) {
	var row dbgen.MailingSendingProfile
	var credential []byte
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err = q.GetMailingSendingProfile(ctx, dbgen.GetMailingSendingProfileParams{BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		if row.SecretRef.Valid {
			if s.Vault == nil {
				return mailprovider.ErrCredentialStorage
			}
			credential, err = s.Vault.Open(ctx, tx, businessID, uuid.UUID(row.SecretRef.Bytes))
			if err != nil {
				return fmt.Errorf("%w: %w", mailprovider.ErrCredentialStorage, err)
			}
			return nil
		}
		return nil
	})
	if err != nil {
		if row.ID != uuid.Nil && errors.Is(err, mailprovider.ErrCredentialStorage) {
			return toSendingProfile(row), mailprovider.Profile{}, err
		}
		return SendingProfile{}, mailprovider.Profile{}, mapErr(err)
	}
	defer clear(credential)
	p := mailprovider.Profile{
		ID: row.ID, UpdatedAt: row.UpdatedAt, Mode: string(row.Mode), FromEmail: row.FromEmail,
		EmailDomainID: uuidPtr(row.EmailDomainID), SESRegion: stringValue(row.SesRegion),
		SESConfigurationSet: stringValue(row.SesConfigurationSet), SNSTopicARN: stringValue(row.SnsTopicArn),
	}
	switch row.Mode {
	case dbgen.MailingSendModeResend:
		var creds resendStoredCredentials
		if err := json.Unmarshal(credential, &creds); err != nil ||
			strings.TrimSpace(creds.APIKey) == "" {
			return toSendingProfile(row), p, mailprovider.ErrCredentials
		}
		if creds.Version == 2 {
			key, keyErr := decodeSvixSecret(strings.TrimSpace(creds.WebhookSecret))
			clear(key)
			if keyErr != nil || strings.TrimSpace(creds.WebhookID) == "" {
				return toSendingProfile(row), p, fmt.Errorf("%w: %w", mailprovider.ErrCredentials, mailprovider.ErrResendWebhook)
			}
			p.ResendWebhookID = creds.WebhookID
		}
		p.ResendAPIKey = creds.APIKey
	case dbgen.MailingSendModeSes:
		var creds SESCredentials
		if err := json.Unmarshal(credential, &creds); err != nil ||
			strings.TrimSpace(creds.AccessKeyID) == "" || strings.TrimSpace(creds.SecretAccessKey) == "" {
			return toSendingProfile(row), p, mailprovider.ErrCredentials
		}
		if err := validateSESFeedbackConfiguration(row.SesRegion, row.SesConfigurationSet, row.SnsTopicArn); err != nil {
			return toSendingProfile(row), p, mailprovider.ErrSESFeedback
		}
		p.SESAccessKeyID, p.SESSecretAccessKey = creds.AccessKeyID, creds.SecretAccessKey
	}
	return toSendingProfile(row), p, nil
}

func (s *Service) checkTestRecipientSuppression(ctx context.Context, principalID, businessID uuid.UUID, recipient string) error {
	var suppressed bool
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		value, err := q.CheckMailingTestRecipientSuppression(ctx, dbgen.CheckMailingTestRecipientSuppressionParams{
			BusinessID: businessID, TenantRootID: root, Email: recipient,
		})
		suppressed = value != nil && *value
		return err
	})
	if err != nil {
		return mapErr(err)
	}
	if suppressed {
		return validation("test recipient is suppressed")
	}
	return nil
}
func providerVerificationMessage(err error) string {
	return mailprovider.VerificationMessage(err)
}

func safeProviderMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	runes := []rune(message)
	if len(runes) > 500 {
		message = string(runes[:500])
	}
	return message
}
