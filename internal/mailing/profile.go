package mailing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

var (
	sesConfigurationSetPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	sesRegionPattern           = regexp.MustCompile(`^[a-z0-9-]{3,32}$`)
	snsAccountPattern          = regexp.MustCompile(`^[0-9]{12}$`)
	snsTopicPattern            = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}(\.fifo)?$`)
)

type resendOperationLease struct {
	profileID    uuid.UUID
	tenantRootID uuid.UUID
	updatedAt    time.Time
	token        uuid.UUID
	apiKey       string
	webhookID    string
}

func (s *Service) GetSendingProfile(ctx context.Context, principalID, businessID uuid.UUID) (SendingProfile, error) {
	var out SendingProfile
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.GetMailingSendingProfile(ctx, dbgen.GetMailingSendingProfileParams{BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		out = toSendingProfile(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) PutSendingProfile(ctx context.Context, principalID, businessID uuid.UUID, in SendingProfileInput) (SendingProfile, error) {
	in.Mode = strings.ToLower(strings.TrimSpace(in.Mode))
	email, err := normalizeEmail(in.FromEmail)
	if err != nil {
		return SendingProfile{}, err
	}
	in.FromEmail = email
	in.FromName = strings.TrimSpace(in.FromName)
	if in.FromName == "" || len(in.FromName) > 200 || strings.ContainsAny(in.FromName, "\r\n") {
		return SendingProfile{}, validation("from_name is required, must be one line, and must not exceed 200 characters")
	}
	if in.ReplyTo != nil {
		reply, err := normalizeEmail(*in.ReplyTo)
		if err != nil {
			return SendingProfile{}, validation("invalid reply_to")
		}
		in.ReplyTo = &reply
	}
	in.PostalAddress = cleanOptional(in.PostalAddress)
	if in.Mode != "relay" && in.Mode != "resend" && in.Mode != "ses" {
		return SendingProfile{}, validation("mode must be relay, resend, or ses")
	}
	if (in.Mode == "relay" && (in.Resend != nil || in.SES != nil)) ||
		(in.Mode == "resend" && in.SES != nil) || (in.Mode == "ses" && in.Resend != nil) {
		return SendingProfile{}, validation("credentials must match the selected mode")
	}
	if in.Mode != "ses" && (in.SESRegion != nil || in.SESConfigurationSet != nil || in.SNSTopicARN != nil) {
		return SendingProfile{}, validation("SES settings are only valid for SES mode")
	}
	if in.Mode == "ses" {
		in.SESRegion = cleanOptional(in.SESRegion)
		in.SESConfigurationSet = cleanOptional(in.SESConfigurationSet)
		in.SNSTopicARN = cleanOptional(in.SNSTopicARN)
		if err := validateSESFeedbackConfiguration(in.SESRegion, in.SESConfigurationSet, in.SNSTopicARN); err != nil {
			return SendingProfile{}, err
		}
	}
	var credential []byte
	if in.Resend != nil {
		if strings.TrimSpace(in.Resend.APIKey) == "" {
			return SendingProfile{}, validation("resend api_key is required")
		}
		credential, err = json.Marshal(resendStoredCredentials{APIKey: strings.TrimSpace(in.Resend.APIKey)})
	}
	if in.SES != nil {
		if strings.TrimSpace(in.SES.AccessKeyID) == "" || strings.TrimSpace(in.SES.SecretAccessKey) == "" {
			return SendingProfile{}, validation("SES credentials are required")
		}
		credential, err = json.Marshal(in.SES)
	}
	if err != nil {
		return SendingProfile{}, validation("invalid credentials")
	}
	lease, err := s.claimResendMutation(ctx, principalID, businessID)
	if err != nil {
		return SendingProfile{}, err
	}
	if lease != nil && lease.webhookID != "" {
		if err = s.deleteResendWebhook(ctx, *lease); err != nil {
			s.releaseResendMutation(ctx, principalID, *lease)
			return SendingProfile{}, err
		}
	}
	var out SendingProfile
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		existing, existingErr := q.GetMailingSendingProfile(ctx, dbgen.GetMailingSendingProfileParams{BusinessID: businessID, TenantRootID: root})
		hasExisting := existingErr == nil
		if existingErr != nil && !errors.Is(existingErr, pgx.ErrNoRows) {
			return existingErr
		}
		if lease != nil {
			if !hasExisting || existing.ID != lease.profileID {
				return fmt.Errorf("mailing: profile changed during Resend cleanup: %w", errs.ErrConflict)
			}
			var heldToken uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT resend_provisioning_token
				FROM mailing_sending_profile WHERE id=$1 AND tenant_root_id=$2 FOR UPDATE`,
				lease.profileID, lease.tenantRootID).Scan(&heldToken); err != nil || heldToken != lease.token {
				return fmt.Errorf("mailing: Resend cleanup lease lost: %w", errs.ErrConflict)
			}
		}
		if hasExisting && existing.Mode == dbgen.MailingSendModeResend &&
			in.Mode == "resend" && in.Resend == nil {
			if lease == nil || strings.TrimSpace(lease.apiKey) == "" {
				return errors.New("mailing: stored Resend credentials are invalid")
			}
			credential, err = json.Marshal(resendStoredCredentials{APIKey: lease.apiKey})
			if err != nil {
				return errors.New("mailing: encode Resend credentials")
			}
		}
		var domainRef, secretRef pgtype.UUID
		if in.Mode == "relay" {
			if in.EmailDomainID == nil {
				return validation("email_domain_id is required for relay")
			}
			domain, err := q.GetEmailDomain(ctx, dbgen.GetEmailDomainParams{ID: *in.EmailDomainID, BusinessID: businessID})
			if err != nil {
				return err
			}
			if !domain.VerifiedAt.Valid {
				return validation("email domain is not verified")
			}
			parts := strings.Split(in.FromEmail, "@")
			if len(parts) != 2 || !strings.EqualFold(parts[1], domain.Domain) {
				return validation("from_email must use the selected verified domain")
			}
			domainRef = pgUUIDPtr(in.EmailDomainID)
		} else {
			if in.EmailDomainID != nil {
				return validation("email_domain_id is only valid for relay")
			}
			if in.Mode == "resend" && in.Resend == nil && (!hasExisting || existing.Mode != dbgen.MailingSendModeResend) {
				return validation("resend credentials are required")
			}
			if in.Mode == "ses" {
				if in.SES == nil && (!hasExisting || existing.Mode != dbgen.MailingSendModeSes) {
					return validation("SES credentials are required")
				}
				if in.SESRegion == nil || strings.TrimSpace(*in.SESRegion) == "" {
					return validation("ses_region is required")
				}
			}
			if len(credential) > 0 {
				if s.Vault == nil {
					return validation("mailing credential storage is not configured")
				}
				id, err := s.Vault.Put(ctx, tx, businessID, "mailing", credential)
				if err != nil {
					return err
				}
				secretRef = pgUUIDPtr(&id)
			} else if hasExisting && existing.SecretRef.Valid {
				secretRef = existing.SecretRef
			} else {
				return validation("provider credentials are required")
			}
		}
		params := dbgen.UpdateMailingSendingProfileParams{
			BusinessID: businessID, TenantRootID: root,
			Mode: dbgen.MailingSendMode(in.Mode), FromEmail: in.FromEmail,
			FromName: in.FromName, ReplyTo: in.ReplyTo,
			PostalAddress: in.PostalAddress, EmailDomainID: domainRef,
			SecretRef: secretRef, SesRegion: cleanOptional(in.SESRegion),
			SesConfigurationSet: cleanOptional(in.SESConfigurationSet),
			SnsTopicArn:         cleanOptional(in.SNSTopicARN),
		}
		var row dbgen.MailingSendingProfile
		if hasExisting {
			row, err = q.UpdateMailingSendingProfile(ctx, params)
		} else {
			row, err = q.InsertMailingSendingProfile(ctx, dbgen.InsertMailingSendingProfileParams{
				ID: uuid.New(), BusinessID: businessID, TenantRootID: root,
				Mode: params.Mode, FromEmail: params.FromEmail, FromName: params.FromName,
				ReplyTo: params.ReplyTo, PostalAddress: params.PostalAddress,
				EmailDomainID: params.EmailDomainID, SecretRef: params.SecretRef,
				SesRegion: params.SesRegion, SesConfigurationSet: params.SesConfigurationSet,
				SnsTopicArn: params.SnsTopicArn,
			})
		}
		if err != nil {
			return err
		}
		if hasExisting && existing.SecretRef.Valid && (!secretRef.Valid || existing.SecretRef.Bytes != secretRef.Bytes) {
			old := uuid.UUID(existing.SecretRef.Bytes)
			if s.Vault == nil {
				return validation("mailing credential storage is not configured")
			}
			if err := s.Vault.Delete(ctx, tx, businessID, old); err != nil && !errors.Is(err, errs.ErrNotFound) {
				return err
			}
		}
		action := "mailing.sending_profile.created"
		if hasExisting {
			action = "mailing.sending_profile.updated"
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root, action, "mailing_sending_profile", row.ID, map[string]any{"mode": row.Mode, "has_credentials": row.SecretRef.Valid}); err != nil {
			return err
		}
		out = toSendingProfile(row)
		return nil
	})
	if err != nil && lease != nil {
		s.releaseResendMutation(ctx, principalID, *lease)
	}
	if err == nil && s.Providers != nil && out.ID != uuid.Nil {
		s.Providers.Invalidate(out.ID)
	}
	return out, mapErr(err)
}

func (s *Service) DeleteSendingProfile(ctx context.Context, principalID, businessID uuid.UUID) error {
	lease, err := s.claimResendMutation(ctx, principalID, businessID)
	if err != nil {
		return err
	}
	if lease != nil && lease.webhookID != "" {
		if err = s.deleteResendWebhook(ctx, *lease); err != nil {
			s.releaseResendMutation(ctx, principalID, *lease)
			return err
		}
	}
	var profileID uuid.UUID
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if lease != nil {
			var heldToken uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT resend_provisioning_token
				FROM mailing_sending_profile WHERE id=$1 AND tenant_root_id=$2 FOR UPDATE`,
				lease.profileID, lease.tenantRootID).Scan(&heldToken); err != nil || heldToken != lease.token {
				return fmt.Errorf("mailing: Resend cleanup lease lost: %w", errs.ErrConflict)
			}
		}
		row, err := q.DeleteMailingSendingProfile(ctx, dbgen.DeleteMailingSendingProfileParams{
			BusinessID: businessID, TenantRootID: root,
		})
		if err != nil {
			return err
		}
		profileID = row.ID
		if err = auditMutation(ctx, tx, principalID, businessID, root,
			"mailing.sending_profile.deleted", "mailing_sending_profile", row.ID,
			map[string]any{"mode": row.Mode}); err != nil {
			return err
		}
		if row.SecretRef.Valid {
			if s.Vault == nil {
				return validation("mailing credential storage is not configured")
			}
			if err = s.Vault.Delete(ctx, tx, businessID, uuid.UUID(row.SecretRef.Bytes)); err != nil &&
				!errors.Is(err, errs.ErrNotFound) {
				return err
			}
		}
		return nil
	})
	if err != nil && lease != nil {
		s.releaseResendMutation(ctx, principalID, *lease)
	}
	if err == nil && s.Providers != nil && profileID != uuid.Nil {
		s.Providers.Invalidate(profileID)
	}
	return mapErr(err)
}

func (s *Service) claimResendMutation(
	ctx context.Context, principalID, businessID uuid.UUID,
) (*resendOperationLease, error) {
	var lease *resendOperationLease
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.GetMailingSendingProfile(ctx, dbgen.GetMailingSendingProfileParams{
			BusinessID: businessID, TenantRootID: root,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil || row.Mode != dbgen.MailingSendModeResend {
			return err
		}
		if s.Vault == nil || !row.SecretRef.Valid {
			return validation("mailing credential storage is not configured")
		}
		raw, err := s.Vault.Open(ctx, tx, businessID, uuid.UUID(row.SecretRef.Bytes))
		if err != nil {
			return err
		}
		var stored resendStoredCredentials
		decodeErr := json.Unmarshal(raw, &stored)
		clear(raw)
		if decodeErr != nil || strings.TrimSpace(stored.APIKey) == "" {
			return errors.New("mailing: stored Resend credentials are invalid")
		}
		token := uuid.New()
		if _, err = q.ClaimMailingResendProvisioning(ctx, dbgen.ClaimMailingResendProvisioningParams{
			Token: token, ID: row.ID, TenantRootID: row.TenantRootID,
			ExpectedUpdatedAt: row.UpdatedAt,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("mailing: Resend profile operation already in progress: %w", errs.ErrConflict)
			}
			return err
		}
		lease = &resendOperationLease{
			profileID: row.ID, tenantRootID: row.TenantRootID, updatedAt: row.UpdatedAt,
			token: token, apiKey: stored.APIKey,
		}
		if stored.Version == 2 {
			lease.webhookID = stored.WebhookID
		}
		return nil
	})
	return lease, mapErr(err)
}

func (s *Service) deleteResendWebhook(ctx context.Context, lease resendOperationLease) error {
	if s.Providers == nil {
		return errors.New("mailing: Resend webhook cleanup provider is not configured")
	}
	deliverer, err := s.Providers.Resolve(ctx, mailprovider.Profile{
		ID: lease.profileID, UpdatedAt: lease.updatedAt,
		Mode: "resend", ResendAPIKey: lease.apiKey,
	})
	if err != nil {
		return err
	}
	provisioner, ok := deliverer.(mailprovider.ResendWebhookProvisioner)
	if !ok {
		return errors.New("mailing: provider does not support Resend webhook cleanup")
	}
	return provisioner.DeleteWebhook(ctx, lease.webhookID)
}

func (s *Service) releaseResendMutation(
	ctx context.Context, principalID uuid.UUID, lease resendOperationLease,
) {
	_ = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).ReleaseMailingResendProvisioning(ctx, dbgen.ReleaseMailingResendProvisioningParams{
			ID: lease.profileID, TenantRootID: lease.tenantRootID, Token: lease.token,
		})
		return err
	})
}

func toSendingProfile(r dbgen.MailingSendingProfile) SendingProfile {
	return SendingProfile{
		ID: r.ID, BusinessID: r.BusinessID, TenantRootID: r.TenantRootID,
		Mode: string(r.Mode), FromEmail: r.FromEmail, FromName: r.FromName,
		ReplyTo: r.ReplyTo, PostalAddress: r.PostalAddress,
		EmailDomainID: uuidPtr(r.EmailDomainID), SESRegion: r.SesRegion,
		SESConfigurationSet: r.SesConfigurationSet, SNSTopicARN: r.SnsTopicArn,
		Status: r.Status, LastVerifiedAt: timePtr(r.LastVerifiedAt),
		VerifyError: r.VerifyError, FeedbackStatus: r.FeedbackStatus,
		FeedbackError: r.FeedbackError, FeedbackConfirmedAt: timePtr(r.FeedbackConfirmedAt),
		HasCredentials: r.SecretRef.Valid, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func validateSESFeedbackConfiguration(region, configurationSet, topicARN *string) error {
	if region == nil || !sesRegionPattern.MatchString(*region) {
		return validation("ses_region is required and must be a valid AWS region")
	}
	if configurationSet == nil || !sesConfigurationSetPattern.MatchString(*configurationSet) {
		return validation("ses_configuration_set is required and invalid")
	}
	if topicARN == nil {
		return validation("sns_topic_arn is required")
	}
	parts := strings.Split(*topicARN, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "sns" ||
		parts[3] != *region || !snsAccountPattern.MatchString(parts[4]) ||
		!snsTopicPattern.MatchString(parts[5]) {
		return validation("sns_topic_arn must bind the SES region and AWS account")
	}
	switch parts[1] {
	case "aws":
		if strings.HasPrefix(*region, "cn-") || strings.HasPrefix(*region, "us-gov-") {
			return validation("sns_topic_arn partition does not match ses_region")
		}
	case "aws-cn":
		if !strings.HasPrefix(*region, "cn-") {
			return validation("sns_topic_arn partition does not match ses_region")
		}
	case "aws-us-gov":
		if !strings.HasPrefix(*region, "us-gov-") {
			return validation("sns_topic_arn partition does not match ses_region")
		}
	default:
		return validation("sns_topic_arn partition is unsupported")
	}
	return nil
}
