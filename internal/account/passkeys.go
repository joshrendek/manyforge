package account

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/weborigin"
)

const (
	passkeyTTL          = 5 * time.Minute
	passkeyFreshness    = 10 * time.Minute
	maxAccountPasskeys  = 20
	passkeyRegistration = "register"
	passkeyLogin        = "login"
)

// Passkeys holds the WebAuthn configuration for the instance's browser origin.
type Passkeys struct {
	server *webauthn.WebAuthn
	origin string
	secure bool
}

// NewPasskeys configures the relying party from the public instance URL. An
// unset URL leaves passkeys unavailable. Local development can use localhost.
func NewPasskeys(publicURL string) (*Passkeys, error) {
	if publicURL == "" {
		return nil, nil
	}
	origin, err := weborigin.Normalize(publicURL)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(origin)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(u.Hostname()) != nil {
		return nil, errors.New("the passkey origin must use a hostname")
	}
	timeout := webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyTTL}
	server, err := webauthn.New(&webauthn.Config{
		RPID:                  u.Hostname(),
		RPDisplayName:         "ManyForge",
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{Login: timeout, Registration: timeout},
	})
	if err != nil {
		return nil, err
	}
	return &Passkeys{server: server, origin: origin, secure: u.Scheme == "https"}, nil
}

type passkeyUser struct {
	account     dbgen.Account
	credentials webauthn.Credentials
}

func (u passkeyUser) WebAuthnID() []byte                         { return u.account.ID[:] }
func (u passkeyUser) WebAuthnName() string                       { return u.account.Email }
func (u passkeyUser) WebAuthnDisplayName() string                { return u.account.DisplayName }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

type passkeySummary struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

func summarizePasskey(id uuid.UUID, name string, createdAt time.Time, lastUsedAt pgtype.Timestamptz) passkeySummary {
	summary := passkeySummary{ID: id, Name: name, CreatedAt: createdAt}
	if lastUsedAt.Valid {
		summary.LastUsedAt = &lastUsedAt.Time
	}
	return summary
}

func (s *Service) isRecentPasskeyLogin(login auth.RecentLogin) bool {
	age := s.now().Sub(login.AuthenticatedAt)
	return login.RefreshID != uuid.Nil && age >= 0 && age < passkeyFreshness
}

// lockPasskeyLogin checks the live login session and requires passkey
// authentication after enrollment. Within a mutation transaction the lock holds through commit.
func (s *Service) lockPasskeyLogin(ctx context.Context, q *dbgen.Queries, principalID uuid.UUID, login auth.RecentLogin, enrolled bool) error {
	if !s.isRecentPasskeyLogin(login) || (enrolled && login.Method != auth.LoginPasskey) {
		return errs.ErrReauthenticationRequired
	}
	_, err := q.LockActiveLoginRefresh(ctx, dbgen.LockActiveLoginRefreshParams{ID: login.RefreshID, PrincipalID: principalID})
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.ErrReauthenticationRequired
	}
	return err
}

func accountForPasskeys(ctx context.Context, q *dbgen.Queries, principalID uuid.UUID) (dbgen.Account, error) {
	account, err := q.GetAccountByPrincipal(ctx, principalID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && account.Status != "active") {
		return dbgen.Account{}, errs.ErrNotFound
	}
	return account, err
}

type passkeyList struct {
	Items     []passkeySummary `json:"items"`
	CanManage bool             `json:"can_manage"`
}

func (s *Service) listPasskeys(ctx context.Context, principalID uuid.UUID, login auth.RecentLogin) (passkeyList, error) {
	q := dbgen.New(s.DB.Pool())
	account, err := accountForPasskeys(ctx, q, principalID)
	if err != nil {
		return passkeyList{}, err
	}
	rows, err := q.ListAccountPasskeys(ctx, account.ID)
	if err != nil {
		return passkeyList{}, err
	}
	items := make([]passkeySummary, 0, len(rows))
	for _, row := range rows {
		items = append(items, summarizePasskey(row.ID, row.Name, row.CreatedAt, row.LastUsedAt))
	}
	err = s.lockPasskeyLogin(ctx, q, principalID, login, len(rows) > 0)
	if err != nil && !errors.Is(err, errs.ErrReauthenticationRequired) {
		return passkeyList{}, err
	}
	return passkeyList{Items: items, CanManage: err == nil}, nil
}

func (s *Service) beginPasskeyRegistration(ctx context.Context, principalID uuid.UUID, login auth.RecentLogin) (*protocol.CredentialCreation, string, error) {
	q := dbgen.New(s.DB.Pool())
	account, err := accountForPasskeys(ctx, q, principalID)
	if err != nil {
		return nil, "", err
	}
	rows, err := q.ListAccountPasskeyCredentials(ctx, account.ID)
	if err != nil {
		return nil, "", err
	}
	if err := s.lockPasskeyLogin(ctx, q, principalID, login, len(rows) > 0); err != nil {
		return nil, "", err
	}
	if len(rows) >= maxAccountPasskeys {
		return nil, "", errs.ErrConflict
	}
	user := passkeyUser{account: account}
	for _, row := range rows {
		var credential webauthn.Credential
		if _, err := credential.UnmarshalMsg(row); err != nil {
			return nil, "", fmt.Errorf("decode passkey: %w", err)
		}
		user.credentials = append(user.credentials, credential)
	}
	options, session, err := s.Passkeys.server.BeginRegistration(user,
		webauthn.WithExclusions(user.credentials.CredentialDescriptors()))
	if err != nil {
		return nil, "", err
	}
	token, err := s.storePasskeyChallenge(ctx, session, db.PGUUID(account.ID), passkeyRegistration)
	return options, token, err
}

func (s *Service) finishPasskeyRegistration(ctx context.Context, principalID uuid.UUID, login auth.RecentLogin, token, name string, response []byte) (passkeySummary, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 80 ||
		strings.ContainsFunc(name, unicode.IsControl) {
		return passkeySummary{}, fmt.Errorf("enter a passkey name of 1 to 80 characters: %w", errs.ErrValidation)
	}
	q := dbgen.New(s.DB.Pool())
	account, err := accountForPasskeys(ctx, q, principalID)
	if err != nil {
		return passkeySummary{}, err
	}
	session, err := s.consumePasskeyChallenge(ctx, token, db.PGUUID(account.ID), passkeyRegistration)
	if errors.Is(err, ErrInvalidCredentials) {
		return passkeySummary{}, fmt.Errorf("passkey setup expired or was already used: %w", errs.ErrValidation)
	}
	if err != nil {
		return passkeySummary{}, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return passkeySummary{}, fmt.Errorf("invalid passkey response: %w", errs.ErrValidation)
	}
	credential, err := s.Passkeys.server.CreateCredential(passkeyUser{account: account}, session, parsed)
	if err != nil {
		return passkeySummary{}, fmt.Errorf("passkey verification failed: %w", errs.ErrValidation)
	}
	record, err := credential.MarshalMsg(nil)
	if err != nil {
		return passkeySummary{}, err
	}
	var summary passkeySummary
	err = s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.LockActiveAccount(ctx, account.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errs.ErrNotFound
			}
			return err
		}
		rows, err := q.ListAccountPasskeys(ctx, account.ID)
		if err != nil {
			return err
		}
		if err := s.lockPasskeyLogin(ctx, q, principalID, login, len(rows) > 0); err != nil {
			return err
		}
		if len(rows) >= maxAccountPasskeys {
			return errs.ErrConflict
		}
		row, err := q.CreateAccountPasskey(ctx, dbgen.CreateAccountPasskeyParams{
			ID: uuid.New(), AccountID: account.ID, CredentialID: credential.ID, Credential: record, Name: name,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return errs.ErrConflict
		}
		if err != nil {
			return err
		}
		summary = summarizePasskey(row.ID, row.Name, row.CreatedAt, row.LastUsedAt)
		target := "account_passkey"
		return audit.Write(ctx, tx, audit.Entry{
			ActorPrincipalID: &principalID, Action: "account.passkey.added", TargetType: &target, TargetID: &row.ID,
		})
	})
	return summary, err
}

func (s *Service) removePasskey(ctx context.Context, principalID uuid.UUID, login auth.RecentLogin, passkeyID uuid.UUID, allowLast bool) error {
	return s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		account, err := accountForPasskeys(ctx, q, principalID)
		if err != nil {
			return err
		}
		if _, err := q.LockActiveAccount(ctx, account.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errs.ErrNotFound
			}
			return err
		}
		if err := s.lockPasskeyLogin(ctx, q, principalID, login, true); err != nil {
			return err
		}
		removed, err := q.RemoveAccountPasskey(ctx, dbgen.RemoveAccountPasskeyParams{AccountID: account.ID, ID: passkeyID})
		if err != nil {
			return err
		}
		if removed == 0 {
			return errs.ErrNotFound
		}
		if !allowLast {
			enrolled, err := q.AccountHasPasskeys(ctx, account.ID)
			if err != nil {
				return err
			}
			if !enrolled {
				return errs.ErrConflict
			}
		}
		target := "account_passkey"
		return audit.Write(ctx, tx, audit.Entry{
			ActorPrincipalID: &principalID, Action: "account.passkey.removed", TargetType: &target, TargetID: &passkeyID,
		})
	})
}

func (s *Service) beginPasskeyLogin(ctx context.Context) (*protocol.CredentialAssertion, string, error) {
	options, session, err := s.Passkeys.server.BeginDiscoverableLogin(
		webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, "", err
	}
	token, err := s.storePasskeyChallenge(ctx, session, pgtype.UUID{}, passkeyLogin)
	return options, token, err
}

func (s *Service) finishPasskeyLogin(ctx context.Context, token string, response []byte) (TokenPair, error) {
	session, err := s.consumePasskeyChallenge(ctx, token, pgtype.UUID{}, passkeyLogin)
	if err != nil {
		return TokenPair{}, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return TokenPair{}, ErrInvalidCredentials
	}
	accountID, err := uuid.FromBytes(parsed.Response.UserHandle)
	if err != nil {
		return TokenPair{}, ErrInvalidCredentials
	}
	var pair TokenPair
	err = s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		account, err := q.LockActiveAccount(ctx, accountID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCredentials
		}
		if err != nil {
			return err
		}
		record, err := q.GetAccountPasskeyCredential(ctx, dbgen.GetAccountPasskeyCredentialParams{
			AccountID: account.ID, CredentialID: parsed.RawID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCredentials
		}
		if err != nil {
			return err
		}
		var stored webauthn.Credential
		if _, err := stored.UnmarshalMsg(record); err != nil {
			return fmt.Errorf("decode passkey: %w", err)
		}
		_, credential, err := s.Passkeys.server.ValidatePasskeyLogin(func(_, _ []byte) (webauthn.User, error) {
			return passkeyUser{account: account, credentials: webauthn.Credentials{stored}}, nil
		}, session, parsed)
		// A synced passkey's counter can differ across devices.
		if err != nil || (credential.Authenticator.CloneWarning && !credential.Flags.BackupEligible) {
			return ErrInvalidCredentials
		}
		updated, err := credential.MarshalMsg(nil)
		if err != nil {
			return err
		}
		if err := q.MarkAccountPasskeyUsed(ctx, dbgen.MarkAccountPasskeyUsedParams{
			AccountID: account.ID, CredentialID: credential.ID, Credential: updated,
		}); err != nil {
			return err
		}
		pair, err = s.issueLoginSession(ctx, tx, account.ID, auth.LoginPasskey)
		return err
	})
	return pair, err
}

func (s *Service) storePasskeyChallenge(ctx context.Context, session *webauthn.SessionData, accountID pgtype.UUID, purpose string) (string, error) {
	token, err := auth.NewOpaqueToken()
	if err != nil {
		return "", err
	}
	record, err := session.MarshalMsg(nil)
	if err != nil {
		return "", err
	}
	q := dbgen.New(s.DB.Pool())
	if err := q.DeleteExpiredPasskeyChallenges(ctx); err != nil {
		return "", err
	}
	err = q.CreatePasskeyChallenge(ctx, dbgen.CreatePasskeyChallengeParams{
		TokenHash: auth.HashToken(token), AccountID: accountID, Purpose: purpose, Session: record, ExpiresAt: session.Expires,
	})
	return token, err
}

func (s *Service) consumePasskeyChallenge(ctx context.Context, token string, accountID pgtype.UUID, purpose string) (webauthn.SessionData, error) {
	var session webauthn.SessionData
	if token == "" {
		return session, ErrInvalidCredentials
	}
	// Consume in its own statement so failed verification cannot make a
	// challenge reusable and concurrent finishes cannot both succeed.
	record, err := dbgen.New(s.DB.Pool()).ConsumePasskeyChallenge(ctx, dbgen.ConsumePasskeyChallengeParams{
		TokenHash: auth.HashToken(token), AccountID: accountID, Purpose: purpose,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return session, ErrInvalidCredentials
	}
	if err != nil {
		return session, err
	}
	_, err = session.UnmarshalMsg(record)
	return session, err
}
