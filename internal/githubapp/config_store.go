// Package githubapp holds the instance-level (tenantless) GitHub App
// integration: the sealed App credentials store and, in later tasks, the
// installation/webhook/API surface built on top of it.
package githubapp

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// txRunner is the subset of *db.DB used here — a transaction runner without a
// principal context, since github_app_config is instance-level (no RLS, no
// tenant scoping; see migrations/0080).
type txRunner interface {
	WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

// AppCreds is the plaintext GitHub App identity + secrets supplied once at
// setup time (Save seals them before they ever reach the database).
type AppCreds struct {
	AppID         int64
	Slug          string
	ClientID      string
	ClientSecret  string
	PrivateKeyPEM string
	WebhookSecret string
}

// AppConfig is the plaintext GitHub App identity + secrets as read back from
// the sealed store (Get unseals them).
type AppConfig struct {
	AppID         int64
	Slug          string
	ClientID      string
	ClientSecret  string
	PrivateKeyPEM string
	WebhookSecret string
}

// ConfigStore reads and writes the single-row, non-overwritable instance
// GitHub App config. Secrets are sealed at rest under Sealer's master key
// (MANYFORGE_GITHUB_APP_MASTER_KEY); see migrations/0080_github_app_config.
type ConfigStore struct {
	DB     txRunner
	Sealer *crypto.Sealer
}

// Save seals c's secrets and inserts the single config row. It returns
// errs.ErrConflict if the row already exists — the row is never overwritten
// once set (DB-enforced via ON CONFLICT DO NOTHING; see migrations/0080).
func (s *ConfigStore) Save(ctx context.Context, c AppCreds) error {
	sec, err := s.Sealer.Seal([]byte(c.ClientSecret))
	if err != nil {
		return fmt.Errorf("seal client secret: %w", err)
	}
	key, err := s.Sealer.Seal([]byte(c.PrivateKeyPEM))
	if err != nil {
		return fmt.Errorf("seal private key: %w", err)
	}
	hook, err := s.Sealer.Seal([]byte(c.WebhookSecret))
	if err != nil {
		return fmt.Errorf("seal webhook secret: %w", err)
	}
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		n, err := dbgen.New(tx).InsertGithubAppConfig(ctx, dbgen.InsertGithubAppConfigParams{
			AppID:               c.AppID,
			Slug:                c.Slug,
			ClientID:            c.ClientID,
			SealedClientSecret:  sec,
			SealedPrivateKey:    key,
			SealedWebhookSecret: hook,
		})
		if err != nil {
			return fmt.Errorf("insert github app config: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("github app already configured: %w", errs.ErrConflict)
		}
		return nil
	})
}

// Get loads and unseals the single config row. It returns errs.ErrNotFound if
// the GitHub App has not been configured yet (no row present).
func (s *ConfigStore) Get(ctx context.Context) (AppConfig, error) {
	var out AppConfig
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		row, err := dbgen.New(tx).GetGithubAppConfig(ctx)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errs.ErrNotFound
			}
			return fmt.Errorf("get github app config: %w", err)
		}
		sec, err := s.Sealer.Open(row.SealedClientSecret)
		if err != nil {
			return fmt.Errorf("open client secret: %w", err)
		}
		key, err := s.Sealer.Open(row.SealedPrivateKey)
		if err != nil {
			return fmt.Errorf("open private key: %w", err)
		}
		hook, err := s.Sealer.Open(row.SealedWebhookSecret)
		if err != nil {
			return fmt.Errorf("open webhook secret: %w", err)
		}
		out = AppConfig{
			AppID:         row.AppID,
			Slug:          row.Slug,
			ClientID:      row.ClientID,
			ClientSecret:  string(sec),
			PrivateKeyPEM: string(key),
			WebhookSecret: string(hook),
		}
		return nil
	})
	return out, err
}
