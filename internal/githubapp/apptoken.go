package githubapp

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AppJWT mints a short-lived App-level JWT (RS256) used to authenticate as
// the GitHub App itself — the only credential that can mint per-repo
// installation tokens (MintInstallationToken below). iat is backdated 60s to
// tolerate clock skew with GitHub's servers; exp is capped at 9m, under
// GitHub's 10m hard limit.
func AppJWT(appID int64, privateKeyPEM string, now time.Time) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKeyPEM))
	if err != nil {
		return "", fmt.Errorf("parse app private key: %w", err)
	}
	claims := jwt.RegisteredClaims{
		Issuer:    fmt.Sprintf("%d", appID),
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)), // clock-skew backdate
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),   // <= 10m
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
}

// tokenMinter is the fakeable surface InstallationTokenSource needs from
// Client — satisfied by *Client's MintInstallationToken.
type tokenMinter interface {
	MintInstallationToken(ctx context.Context, installationID int64, appJWT, repoFullName string) (string, time.Time, error)
}

// appConfigGetter is the fakeable surface InstallationTokenSource needs to
// load the App's identity + private key — satisfied by *ConfigStore.
type appConfigGetter interface {
	Get(ctx context.Context) (AppConfig, error)
}

// InstallationTokenSource mints a fresh per-repo installation token on every
// call (no cache — the only caller is runJob, ~once per review; a cached
// token could expire mid-job and 401 PostReview, re-billing the whole run).
type InstallationTokenSource struct {
	Store appConfigGetter
	API   tokenMinter
	Now   func() time.Time
}

// Token mints a fresh installation token scoped to repoFullName. Any
// failure (config load, JWT signing, or the mint HTTP call — including a
// suspended/deleted installation returning 401/403/404) propagates as a
// plain error; there is no terminal-failure classification here, so the
// caller's bounded retry policy handles it.
func (s *InstallationTokenSource) Token(ctx context.Context, installationID int64, repoFullName string) (string, error) {
	cfg, err := s.Store.Get(ctx)
	if err != nil {
		return "", fmt.Errorf("github app config: %w", err)
	}
	appJWT, err := AppJWT(cfg.AppID, cfg.PrivateKeyPEM, s.Now())
	if err != nil {
		return "", err
	}
	tok, _, err := s.API.MintInstallationToken(ctx, installationID, appJWT, repoFullName)
	if err != nil {
		return "", err
	}
	return tok, nil
}
