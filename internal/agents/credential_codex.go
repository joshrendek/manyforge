package agents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/codexoauth"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// codexOAuth is the fakeable OAuth seam (satisfied by *codexoauth.Client).
type codexOAuth interface {
	StartDeviceAuth(context.Context) (codexoauth.DeviceAuth, error)
	PollDeviceToken(context.Context, string) (codexoauth.TokenSet, codexoauth.PollStatus, error)
	ExchangePKCE(context.Context, string, string) (codexoauth.TokenSet, error)
	Refresh(context.Context, string) (codexoauth.TokenSet, error)
	AuthorizeURL(string, string) string
}

// pendingRow / codexCredRow are the store's return shapes (decoupled from dbgen for testability).
type pendingRow struct {
	Flow               string
	SealedDeviceCode   *string
	SealedPKCEVerifier *string
	DefaultModel       string
	BaseURL            *string
	MaxConcurrentLanes int32
	ExpiresAt          time.Time
}

// codexCredRow is the codex credential's return shape (Task 6 uses this for refresh).
type codexCredRow struct {
	SealedKeyRef      *string
	OAuthRefreshToken *string
	OAuthAccessExpiry *time.Time
	ChatGPTAccountID  *string
}

// upsertCredInput is what a successful connect persists.
type upsertCredInput struct {
	SealedAccess       string
	SealedRefresh      string
	AccessExpiry       time.Time
	AccountID          string
	Plan               string
	DefaultModel       string
	BaseURL            *string
	MaxConcurrentLanes int32
}

// codexStore is the persistence seam. The prod impl runs each method in one WithPrincipal tx.
type codexStore interface {
	insertPending(ctx context.Context, pid, bid uuid.UUID, p pendingRow, jti uuid.UUID) error
	// getPendingLocked loads the pending row FOR UPDATE (single-use).
	getPendingLocked(ctx context.Context, pid, bid, jti uuid.UUID) (pendingRow, error)
	// finishConnect upserts the credential and deletes the pending row in ONE tx.
	finishConnect(ctx context.Context, pid, bid, jti uuid.UUID, in upsertCredInput) (uuid.UUID, error)
}

// CodexTokenService owns the codex connect flows + the refresh/mint state machine (Task 6).
type CodexTokenService struct {
	DB         credentialDB
	Sealer     *crypto.Sealer
	OAuth      codexOAuth
	Store      codexStore    // prod: dbCodexStore{DB}; tests: a fake
	PendingTTL time.Duration // default 15m
	LazyMargin time.Duration // default 5m (Task 6)
	Now        func() time.Time
}

// CodexConnectInput is the credential shape to create on a successful connect.
type CodexConnectInput struct {
	DefaultModel       string
	BaseURL            string
	MaxConcurrentLanes int
}

type DeviceStart struct {
	PendingID               uuid.UUID
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                int
	ExpiresIn               int
}
type PKCEStart struct {
	PendingID    uuid.UUID
	AuthorizeURL string
}
type ConnectStatus struct {
	Status       string // pending | approved | expired | denied
	CredentialID uuid.UUID
}

func (s *CodexTokenService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *CodexTokenService) pendingTTL() time.Duration {
	if s.PendingTTL > 0 {
		return s.PendingTTL
	}
	return 15 * time.Minute
}

func (s *CodexTokenService) validateConnect(in CodexConnectInput) error {
	if in.DefaultModel == "" {
		return fmt.Errorf("codex connect requires default_model: %w", errs.ErrValidation)
	}
	return nil
}

// StartDevice begins the device-code flow and stores the pending row.
func (s *CodexTokenService) StartDevice(ctx context.Context, pid, bid uuid.UUID, in CodexConnectInput) (DeviceStart, error) {
	if err := s.validateConnect(in); err != nil {
		return DeviceStart{}, err
	}
	if s.Sealer == nil {
		return DeviceStart{}, fmt.Errorf("agents: AI master key not configured: %w", errs.ErrValidation)
	}
	da, err := s.OAuth.StartDeviceAuth(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "codex device start failed", "err", err)
		return DeviceStart{}, fmt.Errorf("codex device start: %w", errs.ErrUpstream)
	}
	sealedDC, err := s.Sealer.Seal([]byte(da.DeviceCode))
	if err != nil {
		return DeviceStart{}, fmt.Errorf("codex seal device code: %w", err)
	}
	jti := uuid.New()
	row := pendingRow{
		Flow: "device", SealedDeviceCode: &sealedDC, DefaultModel: in.DefaultModel,
		MaxConcurrentLanes: int32(credLanes(in.MaxConcurrentLanes)),
		ExpiresAt:          s.now().Add(s.pendingTTL()),
	}
	if in.BaseURL != "" {
		row.BaseURL = &in.BaseURL
	}
	if err := s.Store.insertPending(ctx, pid, bid, row, jti); err != nil {
		return DeviceStart{}, mapCredErr(err)
	}
	return DeviceStart{
		PendingID: jti, UserCode: da.UserCode, VerificationURI: da.VerificationURI,
		VerificationURIComplete: da.VerificationURIComplete, Interval: da.Interval, ExpiresIn: da.ExpiresIn,
	}, nil
}

// PollDevice polls once; on approval seals + upserts the credential and consumes the pending row.
func (s *CodexTokenService) PollDevice(ctx context.Context, pid, bid, jti uuid.UUID) (ConnectStatus, error) {
	p, err := s.Store.getPendingLocked(ctx, pid, bid, jti)
	if err != nil {
		return ConnectStatus{}, mapCredErr(err)
	}
	if p.Flow != "device" || p.SealedDeviceCode == nil {
		return ConnectStatus{}, fmt.Errorf("codex pending is not a device flow: %w", errs.ErrValidation)
	}
	if s.Sealer == nil {
		return ConnectStatus{}, fmt.Errorf("agents: AI master key not configured: %w", errs.ErrValidation)
	}
	dc, err := s.Sealer.Open(*p.SealedDeviceCode)
	if err != nil {
		return ConnectStatus{}, fmt.Errorf("codex unseal device code: %w", err)
	}
	ts, st, err := s.OAuth.PollDeviceToken(ctx, string(dc))
	if err != nil {
		if errors.Is(err, codexoauth.ErrMissingAccountID) {
			return ConnectStatus{}, fmt.Errorf("codex id_token missing account id: %w", errs.ErrValidation)
		}
		slog.ErrorContext(ctx, "codex device poll failed", "err", err)
		return ConnectStatus{}, fmt.Errorf("codex device poll: %w", errs.ErrUpstream)
	}
	switch st {
	case codexoauth.PollApproved:
		id, err := s.persistConnect(ctx, pid, bid, jti, ts, p)
		if err != nil {
			return ConnectStatus{}, err
		}
		return ConnectStatus{Status: "approved", CredentialID: id}, nil
	case codexoauth.PollExpired:
		return ConnectStatus{Status: "expired"}, nil
	case codexoauth.PollDenied:
		return ConnectStatus{Status: "denied"}, nil
	default:
		return ConnectStatus{Status: "pending"}, nil
	}
}

// StartPKCE begins the paste-redirect flow.
func (s *CodexTokenService) StartPKCE(ctx context.Context, pid, bid uuid.UUID, in CodexConnectInput) (PKCEStart, error) {
	if err := s.validateConnect(in); err != nil {
		return PKCEStart{}, err
	}
	if s.Sealer == nil {
		return PKCEStart{}, fmt.Errorf("agents: AI master key not configured: %w", errs.ErrValidation)
	}
	verifier, challenge, err := codexoauth.NewPKCE()
	if err != nil {
		return PKCEStart{}, err
	}
	sealedV, err := s.Sealer.Seal([]byte(verifier))
	if err != nil {
		return PKCEStart{}, fmt.Errorf("codex seal verifier: %w", err)
	}
	jti := uuid.New()
	row := pendingRow{
		Flow: "pkce", SealedPKCEVerifier: &sealedV, DefaultModel: in.DefaultModel,
		MaxConcurrentLanes: int32(credLanes(in.MaxConcurrentLanes)),
		ExpiresAt:          s.now().Add(s.pendingTTL()),
	}
	if in.BaseURL != "" {
		row.BaseURL = &in.BaseURL
	}
	if err := s.Store.insertPending(ctx, pid, bid, row, jti); err != nil {
		return PKCEStart{}, mapCredErr(err)
	}
	return PKCEStart{PendingID: jti, AuthorizeURL: s.OAuth.AuthorizeURL(challenge, jti.String())}, nil
}

// ExchangePKCE parses the pasted redirect URL, validates state == jti, exchanges the code.
func (s *CodexTokenService) ExchangePKCE(ctx context.Context, pid, bid, jti uuid.UUID, redirectURL string) (ConnectStatus, error) {
	p, err := s.Store.getPendingLocked(ctx, pid, bid, jti)
	if err != nil {
		return ConnectStatus{}, mapCredErr(err)
	}
	if p.Flow != "pkce" || p.SealedPKCEVerifier == nil {
		return ConnectStatus{}, fmt.Errorf("codex pending is not a pkce flow: %w", errs.ErrValidation)
	}
	u, err := url.Parse(redirectURL)
	if err != nil {
		return ConnectStatus{}, fmt.Errorf("codex bad redirect url: %w", errs.ErrValidation)
	}
	q := u.Query()
	if q.Get("state") != jti.String() {
		return ConnectStatus{}, fmt.Errorf("codex state mismatch: %w", errs.ErrValidation)
	}
	code := q.Get("code")
	if code == "" {
		return ConnectStatus{}, fmt.Errorf("codex redirect url missing code: %w", errs.ErrValidation)
	}
	if s.Sealer == nil {
		return ConnectStatus{}, fmt.Errorf("agents: AI master key not configured: %w", errs.ErrValidation)
	}
	verifier, err := s.Sealer.Open(*p.SealedPKCEVerifier)
	if err != nil {
		return ConnectStatus{}, fmt.Errorf("codex unseal verifier: %w", err)
	}
	ts, err := s.OAuth.ExchangePKCE(ctx, code, string(verifier))
	if err != nil {
		if errors.Is(err, codexoauth.ErrMissingAccountID) {
			return ConnectStatus{}, fmt.Errorf("codex id_token missing account id: %w", errs.ErrValidation)
		}
		slog.ErrorContext(ctx, "codex pkce exchange failed", "err", err)
		return ConnectStatus{}, fmt.Errorf("codex pkce exchange: %w", errs.ErrUpstream)
	}
	id, err := s.persistConnect(ctx, pid, bid, jti, ts, p)
	if err != nil {
		return ConnectStatus{}, err
	}
	return ConnectStatus{Status: "approved", CredentialID: id}, nil
}

// persistConnect seals the token set and upserts the credential + consumes the pending row.
func (s *CodexTokenService) persistConnect(ctx context.Context, pid, bid, jti uuid.UUID, ts codexoauth.TokenSet, p pendingRow) (uuid.UUID, error) {
	if ts.Claims.AccountID == "" {
		return uuid.Nil, fmt.Errorf("codex connect: missing account id: %w", errs.ErrValidation)
	}
	if s.Sealer == nil {
		return uuid.Nil, fmt.Errorf("agents: AI master key not configured: %w", errs.ErrValidation)
	}
	sa, err := s.Sealer.Seal([]byte(ts.AccessToken))
	if err != nil {
		return uuid.Nil, fmt.Errorf("codex seal access: %w", err)
	}
	sr, err := s.Sealer.Seal([]byte(ts.RefreshToken))
	if err != nil {
		return uuid.Nil, fmt.Errorf("codex seal refresh: %w", err)
	}
	id, err := s.Store.finishConnect(ctx, pid, bid, jti, upsertCredInput{
		SealedAccess: sa, SealedRefresh: sr, AccessExpiry: ts.Expiry,
		AccountID: ts.Claims.AccountID, Plan: ts.Claims.Plan,
		DefaultModel: p.DefaultModel, BaseURL: p.BaseURL, MaxConcurrentLanes: p.MaxConcurrentLanes,
	})
	if err != nil {
		return uuid.Nil, mapCredErr(err)
	}
	return id, nil
}

// dbCodexStore is the production codexStore: one WithPrincipal tx per method.
type dbCodexStore struct{ DB credentialDB }

func (d dbCodexStore) insertPending(ctx context.Context, pid, bid uuid.UUID, p pendingRow, jti uuid.UUID) error {
	return d.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).InsertCodexPending(ctx, dbgen.InsertCodexPendingParams{
			Jti: jti, BusinessID: bid, Flow: p.Flow,
			SealedDeviceCode: p.SealedDeviceCode, SealedPkceVerifier: p.SealedPKCEVerifier,
			DefaultModel: p.DefaultModel, BaseUrl: p.BaseURL,
			MaxConcurrentLanes: p.MaxConcurrentLanes, ExpiresAt: p.ExpiresAt,
		})
		return err
	})
}

func (d dbCodexStore) getPendingLocked(ctx context.Context, pid, bid, jti uuid.UUID) (pendingRow, error) {
	var out pendingRow
	err := d.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		r, err := dbgen.New(tx).GetCodexPendingForUpdate(ctx, dbgen.GetCodexPendingForUpdateParams{Jti: jti, BusinessID: bid})
		if err != nil {
			return err
		}
		out = pendingRow{
			Flow: r.Flow, SealedDeviceCode: r.SealedDeviceCode, SealedPKCEVerifier: r.SealedPkceVerifier,
			DefaultModel: r.DefaultModel, BaseURL: r.BaseUrl, MaxConcurrentLanes: r.MaxConcurrentLanes,
			ExpiresAt: r.ExpiresAt,
		}
		return nil
	})
	return out, err
}

func (d dbCodexStore) finishConnect(ctx context.Context, pid, bid, jti uuid.UUID, in upsertCredInput) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, err := q.UpsertCodexCredential(ctx, dbgen.UpsertCodexCredentialParams{
			ID: uuid.New(), BusinessID: bid,
			SealedKeyRef: &in.SealedAccess, BaseUrl: in.BaseURL, DefaultModel: in.DefaultModel,
			MaxConcurrentLanes: in.MaxConcurrentLanes, ChatgptAccountID: &in.AccountID,
			OauthRefreshToken: &in.SealedRefresh, OauthAccessExpiry: pgtype.Timestamptz{Time: in.AccessExpiry, Valid: true},
			ChatgptPlan: &in.Plan,
		})
		if err != nil {
			return err
		}
		id = row.ID
		return q.DeleteCodexPending(ctx, dbgen.DeleteCodexPendingParams{Jti: jti, BusinessID: bid})
	})
	return id, err
}
