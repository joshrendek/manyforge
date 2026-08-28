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
	profile, providerProfile, err := s.loadProviderProfile(ctx, principalID, businessID)
	if err != nil {
		return SendingProfile{}, err
	}
	verifyErr := errors.New("provider resolution is not configured")
	if s.Providers != nil {
		var deliverer mailprovider.Deliverer
		deliverer, verifyErr = s.Providers.Resolve(ctx, providerProfile)
		if verifyErr == nil {
			verifier, ok := deliverer.(mailprovider.Verifier)
			if !ok {
				verifyErr = errors.New("provider does not support verification")
			} else {
				verifyErr = verifier.Verify(ctx)
			}
		}
	}
	status, message := "verified", ""
	if verifyErr != nil {
		status, message = "error", safeProviderMessage(verifyErr)
	}

	var out SendingProfile
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		row, err := dbgen.New(tx).SetMailingSendingProfileVerification(ctx, dbgen.SetMailingSendingProfileVerificationParams{
			Status: status, VerifyError: message, ID: profile.ID,
			TenantRootID: profile.TenantRootID, ExpectedUpdatedAt: profile.UpdatedAt,
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

func (s *Service) TestSendingProfile(ctx context.Context, principalID, businessID uuid.UUID, recipient string) error {
	to, err := normalizeEmail(recipient)
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
				return errors.New("mailing: credential storage is not configured")
			}
			credential, err = s.Vault.Open(ctx, tx, businessID, uuid.UUID(row.SecretRef.Bytes))
			return err
		}
		return nil
	})
	if err != nil {
		return SendingProfile{}, mailprovider.Profile{}, mapErr(err)
	}
	defer clear(credential)
	p := mailprovider.Profile{
		ID: row.ID, UpdatedAt: row.UpdatedAt, Mode: string(row.Mode), FromEmail: row.FromEmail,
		EmailDomainID: uuidPtr(row.EmailDomainID), SESRegion: stringValue(row.SesRegion),
		SESConfigurationSet: stringValue(row.SesConfigurationSet),
	}
	switch row.Mode {
	case dbgen.MailingSendModeResend:
		var creds ResendCredentials
		if err := json.Unmarshal(credential, &creds); err != nil {
			return SendingProfile{}, mailprovider.Profile{}, errors.New("mailing: stored Resend credentials are invalid")
		}
		p.ResendAPIKey = creds.APIKey
	case dbgen.MailingSendModeSes:
		var creds SESCredentials
		if err := json.Unmarshal(credential, &creds); err != nil {
			return SendingProfile{}, mailprovider.Profile{}, errors.New("mailing: stored SES credentials are invalid")
		}
		p.SESAccessKeyID, p.SESSecretAccessKey = creds.AccessKeyID, creds.SecretAccessKey
	}
	return toSendingProfile(row), p, nil
}

func safeProviderMessage(err error) string {
	message := strings.TrimSpace(err.Error())
	runes := []rune(message)
	if len(runes) > 500 {
		message = string(runes[:500])
	}
	return message
}
