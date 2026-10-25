package ticketing

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

func (s *PortalService) Contact(ctx context.Context, p PortalScope) (PortalContact, error) {
	out := PortalContact{}
	err := s.withScope(ctx, p, func(tx pgx.Tx) error {
		var email *string
		err := tx.QueryRow(ctx, `SELECT verified_email FROM support_portal_contact WHERE business_id=$1 AND tenant_root_id=$2 AND external_id=$3`, p.BusinessID, p.TenantRootID, p.ExternalID).Scan(&email)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if email != nil {
			out.Email = *email
			out.Verified = true
		}
		return nil
	})
	return out, err
}

func portalEnsureContact(ctx context.Context, tx pgx.Tx, p PortalScope) error {
	_, err := tx.Exec(ctx, `INSERT INTO support_portal_contact(business_id,tenant_root_id,external_id,display_name) VALUES($1,$2,$3,$4) ON CONFLICT(business_id,external_id) DO UPDATE SET display_name=EXCLUDED.display_name`, p.BusinessID, p.TenantRootID, p.ExternalID, p.DisplayName)
	return err
}

func (s *PortalService) SendContactVerification(ctx context.Context, p PortalScope, email string) error {
	canonical, err := portalEmail(email)
	if err != nil {
		return err
	}
	if s.Mailer == nil {
		return ErrPortalUnavailable
	}
	codeNumber, err := rand.Int(rand.Reader, big.NewInt(100000000))
	if err != nil {
		return ErrPortalUnavailable
	}
	code := fmt.Sprintf("%08d", codeNumber.Int64())
	nonce := uuid.New()
	var profile uuid.UUID
	var from, inbound string
	// Commit the allowance before provider I/O. Cancellation, ambiguous delivery,
	// or a failed final commit must never roll the send budget back.
	err = s.withScope(ctx, p, func(tx pgx.Tx) error {
		if err := portalEnsureContact(ctx, tx, p); err != nil {
			return err
		}
		var last *time.Time
		var sends []time.Time
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT last_sent_at,ARRAY(SELECT sent FROM unnest(send_times) sent WHERE sent>clock_timestamp()-interval '1 hour'),clock_timestamp() FROM support_portal_contact WHERE business_id=$1 AND tenant_root_id=$2 AND external_id=$3 FOR UPDATE`, p.BusinessID, p.TenantRootID, p.ExternalID).Scan(&last, &sends, &now); err != nil {
			return err
		}
		if last != nil && now.Sub(*last) < time.Minute {
			return ErrPortalRate
		}
		if len(sends) >= 5 {
			return ErrPortalRate
		}
		var err error
		profile, from, inbound, err = portalSender(ctx, tx, p)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE support_portal_contact SET send_nonce=$5,last_sent_at=clock_timestamp(),send_times=array_append($4::timestamptz[],clock_timestamp()) WHERE business_id=$1 AND tenant_root_id=$2 AND external_id=$3`, p.BusinessID, p.TenantRootID, p.ExternalID, sends, nonce)
		return err
	})
	if err != nil {
		return err
	}
	mailCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.Mailer.SendSupportReply(mailCtx, profile, notify.Mail{From: from, To: canonical, Subject: "Verify your support contact email", BodyText: "Your support contact verification code is " + code + ". It expires in 10 minutes. If you did not request this, ignore this email.", MessageID: nonce.String() + "@" + strings.SplitN(from, "@", 2)[1], ReplyTo: inbound}); err != nil {
		return ErrPortalUnavailable
	}
	// Revalidate authority and the exact sender after delivery. Preserve the old
	// pending proof unless this accepted send is still the latest reservation.
	return s.withScope(ctx, p, func(tx pgx.Tx) error {
		currentProfile, currentFrom, currentInbound, err := portalSender(ctx, tx, p)
		if err != nil {
			return err
		}
		if currentProfile != profile || currentFrom != from || currentInbound != inbound {
			return ErrPortalUnavailable
		}
		tag, err := tx.Exec(ctx, `UPDATE support_portal_contact SET pending_email=$4,challenge_hash=$5,challenge_nonce=$6,challenge_key_id=$7,challenge_expires_at=clock_timestamp()+interval '10 minutes',attempts=0 WHERE business_id=$1 AND tenant_root_id=$2 AND external_id=$3 AND send_nonce=$6`, p.BusinessID, p.TenantRootID, p.ExternalID, canonical, portalChallengeHash(p.token, p.ExternalID, canonical, nonce, code), nonce, p.KeyID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrPortalConflict
		}
		return nil
	})
}

func portalSender(ctx context.Context, tx pgx.Tx, p PortalScope) (uuid.UUID, string, string, error) {
	var profile uuid.UUID
	var from, status, inbound string
	if err := tx.QueryRow(ctx, `SELECT profile_id,from_address,profile_status,inbound_address FROM get_resend_support_identity($1,$2)`, p.BusinessID, p.TenantRootID).Scan(&profile, &from, &status, &inbound); err != nil {
		return uuid.Nil, "", "", ErrPortalUnavailable
	}
	if status != "verified" || !validSendAs(from) || !validSendAs(inbound) {
		return uuid.Nil, "", "", ErrPortalUnavailable
	}
	var locked bool
	err := tx.QueryRow(ctx, `SELECT true FROM mailing_sending_profile mp JOIN inbound_address ia ON ia.business_id=mp.business_id AND ia.tenant_root_id=mp.tenant_root_id JOIN email_domain ed ON ed.id=ia.email_domain_id AND ed.business_id=mp.business_id AND ed.tenant_root_id=mp.tenant_root_id WHERE mp.id=$1 AND mp.business_id=$2 AND mp.tenant_root_id=$3 AND mp.mode='resend' AND mp.status='verified' AND ia.address::text=$4 AND ia.kind='custom' AND ed.verified_at IS NOT NULL AND ed.mode='provider_route' AND (CASE WHEN ia.send_as IS NOT NULL AND lower(split_part(mp.from_email::text,'@',2))=lower(split_part(ia.send_as::text,'@',2)) THEN ia.send_as ELSE ia.address END)::text=$5 FOR SHARE OF mp,ia,ed`, profile, p.BusinessID, p.TenantRootID, inbound, from).Scan(&locked)
	if err != nil {
		return uuid.Nil, "", "", ErrPortalUnavailable
	}
	return profile, from, inbound, nil
}

func (s *PortalService) VerifyContact(ctx context.Context, p PortalScope, code string) error {
	if len(code) != 8 || strings.IndexFunc(code, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return ErrPortalInvalid
	}
	var verdict error
	err := s.withScope(ctx, p, func(tx pgx.Tx) error {
		var email *string
		var hash []byte
		var nonce, key *uuid.UUID
		var expires *time.Time
		var attempts int
		var now time.Time
		err := tx.QueryRow(ctx, `SELECT pending_email,challenge_hash,challenge_nonce,challenge_key_id,challenge_expires_at,attempts,clock_timestamp() FROM support_portal_contact WHERE business_id=$1 AND tenant_root_id=$2 AND external_id=$3 FOR UPDATE`, p.BusinessID, p.TenantRootID, p.ExternalID).Scan(&email, &hash, &nonce, &key, &expires, &attempts, &now)
		if errors.Is(err, pgx.ErrNoRows) {
			verdict = ErrPortalInvalid
			return nil
		}
		if err != nil {
			return err
		}
		if email == nil || nonce == nil || key == nil || expires == nil || *key != p.KeyID || !now.Before(*expires) || attempts >= 6 {
			verdict = ErrPortalInvalid
			return nil
		}
		if !hmac.Equal(hash, portalChallengeHash(p.token, p.ExternalID, *email, *nonce, code)) {
			_, err := tx.Exec(ctx, `UPDATE support_portal_contact SET attempts=attempts+1 WHERE business_id=$1 AND tenant_root_id=$2 AND external_id=$3`, p.BusinessID, p.TenantRootID, p.ExternalID)
			verdict = ErrPortalInvalid
			return err
		}
		var requester uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT resolve_support_portal_requester($1,$2,$3,$4)`, p.BusinessID, p.TenantRootID, *email, p.DisplayName).Scan(&requester); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE support_portal_contact SET verified_email=$4,requester_id=$5,pending_email=NULL,challenge_hash=NULL,challenge_nonce=NULL,challenge_key_id=NULL,challenge_expires_at=NULL,attempts=0 WHERE business_id=$1 AND tenant_root_id=$2 AND external_id=$3`, p.BusinessID, p.TenantRootID, p.ExternalID, *email, requester); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE ticket t SET requester_id=$4,updated_at=now() WHERE t.business_id=$1 AND t.tenant_root_id=$2 AND EXISTS(SELECT 1 FROM support_portal_ticket pt WHERE pt.ticket_id=t.id AND pt.business_id=$1 AND pt.tenant_root_id=$2 AND pt.external_id=$3)`, p.BusinessID, p.TenantRootID, p.ExternalID, requester); err != nil {
			return err
		}
		return portalAudit(ctx, tx, p.PrincipalID, p.BusinessID, p.TenantRootID, "support.portal_contact.verified", p.KeyID)
	})
	if err != nil {
		return err
	}
	return verdict
}
