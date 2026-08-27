package mailing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

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
	if in.FromName == "" || len(in.FromName) > 200 {
		return SendingProfile{}, validation("from_name is required and must not exceed 200 characters")
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
	var credential []byte
	if in.Resend != nil {
		if strings.TrimSpace(in.Resend.APIKey) == "" {
			return SendingProfile{}, validation("resend api_key is required")
		}
		credential, err = json.Marshal(in.Resend)
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
	return out, mapErr(err)
}

func (s *Service) DeleteSendingProfile(ctx context.Context, principalID, businessID uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.DeleteMailingSendingProfile(ctx, dbgen.DeleteMailingSendingProfileParams{BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root, "mailing.sending_profile.deleted", "mailing_sending_profile", row.ID, map[string]any{"mode": row.Mode}); err != nil {
			return err
		}
		if row.SecretRef.Valid {
			if s.Vault == nil {
				return validation("mailing credential storage is not configured")
			}
			id := uuid.UUID(row.SecretRef.Bytes)
			if err = s.Vault.Delete(ctx, tx, businessID, id); err != nil && !errors.Is(err, errs.ErrNotFound) {
				return err
			}
		}
		return nil
	})
	return mapErr(err)
}

func toSendingProfile(r dbgen.MailingSendingProfile) SendingProfile {
	return SendingProfile{
		ID: r.ID, BusinessID: r.BusinessID, TenantRootID: r.TenantRootID,
		Mode: string(r.Mode), FromEmail: r.FromEmail, FromName: r.FromName,
		ReplyTo: r.ReplyTo, PostalAddress: r.PostalAddress,
		EmailDomainID: uuidPtr(r.EmailDomainID), SESRegion: r.SesRegion,
		SESConfigurationSet: r.SesConfigurationSet, SNSTopicARN: r.SnsTopicArn,
		Status: r.Status, LastVerifiedAt: timePtr(r.LastVerifiedAt),
		VerifyError: r.VerifyError, HasCredentials: r.SecretRef.Valid,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
