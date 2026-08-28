package provider

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

// KeyOpener decrypts the sealed Ed25519 private key stored for a relay domain.
type KeyOpener interface{ Open(string) ([]byte, error) }

// Relay sends through the shared notifier after resolving and attaching the
// selected verified domain's DKIM identity.
type Relay struct {
	DB            *db.DB
	Sealer        KeyOpener
	Sender        notify.Sender
	EmailDomainID uuid.UUID
}

func (r *Relay) Verify(ctx context.Context) error {
	_, err := r.identity(ctx)
	return err
}

func (r *Relay) Send(ctx context.Context, mail notify.Mail) (SendResult, error) {
	identity, err := r.identity(ctx)
	if err != nil {
		return SendResult{}, err
	}
	mail.DKIM = &notify.DKIMConfig{Domain: identity.domain, Selector: identity.selector, PrivateKey: identity.key}
	if err := r.Sender.Send(ctx, mail); err != nil {
		return SendResult{}, err
	}
	return SendResult{ProviderID: mail.MessageID}, nil
}

type relayIdentity struct {
	domain, selector string
	key              ed25519.PrivateKey
}

func (r *Relay) identity(ctx context.Context) (relayIdentity, error) {
	if r.DB == nil || r.Sealer == nil || r.Sender == nil || r.EmailDomainID == uuid.Nil {
		return relayIdentity{}, errors.New("provider: relay is not configured")
	}
	var domain, selector, sealed string
	err := r.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT dkim_domain, dkim_selector, dkim_private_key_ref FROM mailing_relay_identity($1)",
			r.EmailDomainID).Scan(&domain, &selector, &sealed)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return relayIdentity{}, errors.New("provider: relay domain is not verified")
		}
		return relayIdentity{}, fmt.Errorf("provider: relay identity: %w", err)
	}
	key, err := r.Sealer.Open(sealed)
	if err != nil {
		return relayIdentity{}, errors.New("provider: relay DKIM key could not be opened")
	}
	if len(key) != ed25519.PrivateKeySize {
		return relayIdentity{}, errors.New("provider: relay DKIM key is invalid")
	}
	return relayIdentity{domain: domain, selector: selector, key: ed25519.PrivateKey(key)}, nil
}
