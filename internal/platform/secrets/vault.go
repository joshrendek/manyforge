// Package secrets is the SL-B credential vault: it seals secrets at rest with the
// platform Sealer (AES-256-GCM) and stores only ciphertext. Put/Open/Delete run in
// a caller-provided tx so a secret write composes atomically with the domain row +
// audit entry. The vault never logs plaintext and never audits — that is the
// domain service's job.
package secrets

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

// Vault seals + stores secrets. Sealer is the AES-256-GCM sealer.
type Vault struct {
	Sealer *crypto.Sealer
}

// NewVault builds a Vault around a Sealer.
func NewVault(s *crypto.Sealer) *Vault { return &Vault{Sealer: s} }

// Put seals plaintext and inserts a secret row in the caller's tx, returning the new
// secret id. Plaintext is sealed BEFORE the insert; only ciphertext touches the DB.
// The InsertSecret query derives tenant_root + enforces RLS from business_id.
func (v *Vault) Put(ctx context.Context, tx pgx.Tx, businessID uuid.UUID, scope string, plaintext []byte) (uuid.UUID, error) {
	sealed, err := v.Sealer.Seal(plaintext)
	if err != nil {
		return uuid.Nil, fmt.Errorf("secrets: seal: %w", err)
	}
	id := uuid.New()
	if _, err := dbgen.New(tx).InsertSecret(ctx, dbgen.InsertSecretParams{
		ID: id, BusinessID: businessID, Scope: scope, SealedValue: sealed,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("secrets: insert: %w", err)
	}
	return id, nil
}

// Open fetches + unseals the secret by id for the business, in the caller's tx.
func (v *Vault) Open(ctx context.Context, tx pgx.Tx, businessID, secretID uuid.UUID) ([]byte, error) {
	row, err := dbgen.New(tx).GetSecret(ctx, dbgen.GetSecretParams{ID: secretID, BusinessID: businessID})
	if err != nil {
		return nil, fmt.Errorf("secrets: get: %w", err)
	}
	pt, err := v.Sealer.Open(row.SealedValue)
	if err != nil {
		return nil, fmt.Errorf("secrets: open: %w", err)
	}
	return pt, nil
}

// Delete removes the secret in the caller's tx.
func (v *Vault) Delete(ctx context.Context, tx pgx.Tx, businessID, secretID uuid.UUID) error {
	if _, err := dbgen.New(tx).DeleteSecret(ctx, dbgen.DeleteSecretParams{ID: secretID, BusinessID: businessID}); err != nil {
		return fmt.Errorf("secrets: delete: %w", err)
	}
	return nil
}
