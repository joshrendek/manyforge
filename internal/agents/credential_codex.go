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
	ExchangePKCE(context.Context, string, string) (codexoauth.TokenSet, error)
	Refresh(context.Context, string) (codexoauth.TokenSet, error)
	AuthorizeURL(string, string) string
}

// codexModelLister is the seam to the ChatGPT Codex backend's live per-account model list.
// Satisfied by *CodexBackendModels; nil in tests/deployments that don't wire it.
type codexModelLister interface {
	ListModels(ctx context.Context, accessToken, accountID string) ([]string, error)
}

var _ codexModelLister = (*CodexBackendModels)(nil)

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
	ChatGPTPlan       *string
}

// upsertTokens is a freshly-rotated + sealed token set to write back.
type upsertTokens struct {
	SealedAccess  string
	SealedRefresh string
	AccessExpiry  time.Time
	Plan          string
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
	// readCodex is the lazy fast-path read (no lock).
	readCodex(ctx context.Context, pid, bid uuid.UUID) (codexCredRow, error)
	// refreshLockedTx row-locks the codex credential (FOR UPDATE, WithPrincipal), runs fn under
	// the lock, and applies fn's result: *upsertTokens -> UpdateCodexOAuthTokens; disconnect=true
	// -> DisconnectCodexCredential; (nil,false) -> no write. All in ONE tx.
	refreshLockedTx(ctx context.Context, pid, bid uuid.UUID, fn func(codexCredRow) (*upsertTokens, bool, error)) error
}

// claimedCodex is one credential claimed by the cross-tenant sweep (sealed refresh + current plan).
type claimedCodex struct {
	ID            uuid.UUID
	SealedRefresh string
	Plan          string
}

// codexTxRunner is the principal-less tx seam (satisfied by *db.DB via WithTx) — the sweep is
// cross-tenant and runs through SECURITY DEFINER functions, so it must NOT use WithPrincipal.
type codexTxRunner interface {
	WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

// systemCodexStore is the scheduler's RLS-exempt seam (Task 6b).
type systemCodexStore interface {
	// refreshOneDue claims the next due codex credential (excluding ids already handled this
	// sweep), runs fn under the claim's row lock, and applies fn's result — all in ONE WithTx.
	// Returns handled=false when nothing is due. On an fn error the tx rolls back (no write) but
	// the claimed id is still returned so the caller can exclude it from the rest of the sweep.
	refreshOneDue(ctx context.Context, cutoff time.Time, exclude []string,
		fn func(claimedCodex) (*upsertTokens, bool, error)) (id uuid.UUID, handled bool, err error)
}

// CodexTokenService owns the codex connect flows + the refresh/mint state machine (Task 6) and
// the cross-tenant background sweep (Task 6b).
type CodexTokenService struct {
	DB          credentialDB
	Sealer      *crypto.Sealer
	OAuth       codexOAuth
	Models      codexModelLister // live per-account catalog (nil = feature off → static fallback)
	Store       codexStore       // prod: dbCodexStore{DB}; tests: a fake
	SystemStore systemCodexStore // prod: dbSystemCodexStore{DB}; tests: a fake (Task 6b sweep)
	PendingTTL  time.Duration    // default 15m
	LazyMargin  time.Duration    // default 5m (Task 6)
	Now         func() time.Time
}

// CodexConnectInput is the credential shape to create on a successful connect.
type CodexConnectInput struct {
	DefaultModel       string
	BaseURL            string
	MaxConcurrentLanes int
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

func (s *CodexTokenService) lazyMargin() time.Duration {
	if s.LazyMargin > 0 {
		return s.LazyMargin
	}
	return 5 * time.Minute
}

// doRefresh unseals the refresh token, calls OpenAI, and seals the rotated set. Returns
// (tokens,false,nil) on success, (nil,true,nil) on invalid_grant (caller disconnects), or
// (nil,false,ErrUpstream) on any other failure. existingPlan preserves chatgpt_plan when the
// refresh response omits the id_token (Task 4 note: decodeToken leaves Claims empty then).
func (s *CodexTokenService) doRefresh(ctx context.Context, sealedRefresh, existingPlan string) (*upsertTokens, bool, error) {
	rt, err := s.Sealer.Open(sealedRefresh)
	if err != nil {
		return nil, false, fmt.Errorf("codex unseal refresh: %w", err)
	}
	ts, rerr := s.OAuth.Refresh(ctx, string(rt))
	if rerr != nil {
		if errors.Is(rerr, codexoauth.ErrInvalidGrant) {
			return nil, true, nil
		}
		slog.ErrorContext(ctx, "codex token refresh failed", "err", rerr)
		return nil, false, fmt.Errorf("codex refresh: %w", errs.ErrUpstream)
	}
	sa, err := s.Sealer.Seal([]byte(ts.AccessToken))
	if err != nil {
		return nil, false, fmt.Errorf("codex seal access: %w", err)
	}
	sr, err := s.Sealer.Seal([]byte(ts.RefreshToken))
	if err != nil {
		return nil, false, fmt.Errorf("codex seal refresh: %w", err)
	}
	plan := ts.Claims.Plan
	if plan == "" {
		plan = existingPlan
	}
	return &upsertTokens{SealedAccess: sa, SealedRefresh: sr, AccessExpiry: ts.Expiry, Plan: plan}, false, nil
}

// Mint returns a live access token: fast-path read (no lock) if still fresh, else refreshLocked.
func (s *CodexTokenService) Mint(ctx context.Context, pid, bid uuid.UUID) (string, error) {
	if s.Sealer == nil {
		return "", fmt.Errorf("agents: AI master key not configured: %w", errs.ErrValidation)
	}
	row, err := s.Store.readCodex(ctx, pid, bid)
	if err != nil {
		return "", mapCredErr(err)
	}
	if row.OAuthAccessExpiry == nil && row.OAuthRefreshToken == nil && row.SealedKeyRef != nil {
		// Increment-1 manual-token credential: a pasted access token with no OAuth lifecycle.
		// There is nothing to refresh — use the pasted token as-is; if it has since expired the
		// sandbox call fails (that is the Increment-1 behavior, unchanged here).
		tok, oerr := s.Sealer.Open(*row.SealedKeyRef)
		if oerr != nil {
			return "", fmt.Errorf("codex unseal access: %w", oerr)
		}
		return string(tok), nil
	}
	if row.OAuthAccessExpiry != nil && row.SealedKeyRef != nil &&
		s.now().Before(row.OAuthAccessExpiry.Add(-s.lazyMargin())) {
		tok, oerr := s.Sealer.Open(*row.SealedKeyRef)
		if oerr != nil {
			return "", fmt.Errorf("codex unseal access: %w", oerr)
		}
		return string(tok), nil
	}
	return s.refreshLocked(ctx, pid, bid)
}

// refreshLocked row-locks the credential, double-checks freshness, and refreshes under the lock
// (serializing rotation). Returns the fresh access token, or ErrCodexDisconnected on a dead token.
func (s *CodexTokenService) refreshLocked(ctx context.Context, pid, bid uuid.UUID) (string, error) {
	var access string
	err := s.Store.refreshLockedTx(ctx, pid, bid, func(row codexCredRow) (*upsertTokens, bool, error) {
		// double-check: another refresher may have run while we waited for the lock
		if row.OAuthAccessExpiry != nil && row.SealedKeyRef != nil &&
			s.now().Before(row.OAuthAccessExpiry.Add(-s.lazyMargin())) {
			tok, oerr := s.Sealer.Open(*row.SealedKeyRef)
			if oerr != nil {
				return nil, false, oerr
			}
			access = string(tok)
			return nil, false, nil
		}
		if row.OAuthRefreshToken == nil {
			return nil, false, errs.ErrCodexDisconnected // manual-token cred or already cleared
		}
		existingPlan := ""
		if row.ChatGPTPlan != nil {
			existingPlan = *row.ChatGPTPlan
		}
		toks, disconnect, derr := s.doRefresh(ctx, *row.OAuthRefreshToken, existingPlan)
		if derr != nil {
			return nil, false, derr
		}
		if disconnect {
			return nil, true, nil
		}
		accBytes, oerr := s.Sealer.Open(toks.SealedAccess)
		if oerr != nil {
			return nil, false, oerr
		}
		access = string(accBytes)
		return toks, false, nil
	})
	if err != nil {
		return "", err
	}
	if access == "" {
		// fn signalled a disconnect (wrote nulls) without an error → surface the typed sentinel
		return "", errs.ErrCodexDisconnected
	}
	return access, nil
}

// maxCodexSweep bounds one sweep so a persistently-failing credential can't monopolize it.
const maxCodexSweep = 500

// RefreshDue proactively refreshes near-expiry codex credentials across all tenants. Each
// credential is claimed FOR UPDATE SKIP LOCKED and refreshed under that lock; a per-credential
// upstream failure is logged and skipped (excluded from the rest of this sweep), not fatal.
func (s *CodexTokenService) RefreshDue(ctx context.Context) (int, error) {
	if s.Sealer == nil {
		return 0, fmt.Errorf("agents: AI master key not configured: %w", errs.ErrValidation)
	}
	cutoff := s.now().Add(s.lazyMargin())
	var refreshed int
	exclude := make([]string, 0, 8)
	for i := 0; i < maxCodexSweep; i++ {
		var disconnected bool
		id, handled, err := s.SystemStore.refreshOneDue(ctx, cutoff, exclude,
			func(c claimedCodex) (*upsertTokens, bool, error) {
				toks, disc, derr := s.doRefresh(ctx, c.SealedRefresh, c.Plan)
				disconnected = disc
				return toks, disc, derr
			})
		if !handled {
			if err != nil {
				// claim/tx failure (WithTx/Begin, or the claim's QueryRow.Scan failed with
				// something other than pgx.ErrNoRows) — distinct from "nothing due". Surface it
				// so the sweep doesn't silently look empty forever.
				return refreshed, fmt.Errorf("codex refresh sweep claim: %w", err)
			}
			break // nothing left due
		}
		exclude = append(exclude, id.String())
		if err != nil {
			if errors.Is(err, errs.ErrCodexDisconnected) {
				continue // shouldn't reach here (disconnect is applied inside the tx), defensive
			}
			slog.WarnContext(ctx, "codex refresh sweep: credential failed", "id", id, "err", err)
			continue // upstream/transport failure for one credential — skip, keep sweeping
		}
		if disconnected {
			continue // dead refresh token: disconnected inside the tx, not counted as refreshed
		}
		refreshed++
	}
	return refreshed, nil
}

func (s *CodexTokenService) validateConnect(in CodexConnectInput) error {
	if in.DefaultModel == "" {
		return fmt.Errorf("codex connect requires default_model: %w", errs.ErrValidation)
	}
	return nil
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

// ListAccountModels returns the connected account's live, user-visible Codex models (via the
// ChatGPT Codex backend). It is best-effort: any problem (feature off, not a connected OAuth codex
// credential, mint/fetch failure) returns an empty list with a nil error so the caller degrades to
// the static model_pricing catalog rather than erroring the UI.
func (s *CodexTokenService) ListAccountModels(ctx context.Context, pid, bid uuid.UUID) ([]ModelInfo, error) {
	if s.Models == nil {
		return nil, nil
	}
	row, err := s.Store.readCodex(ctx, pid, bid)
	if err != nil {
		return nil, nil // no codex credential / read error → static fallback
	}
	// Only a connected OAuth codex credential has a live per-account catalog (a pasted Increment-1
	// manual token has no refresh token / account id).
	if row.ChatGPTAccountID == nil || *row.ChatGPTAccountID == "" || row.OAuthRefreshToken == nil {
		return nil, nil
	}
	token, err := s.Mint(ctx, pid, bid)
	if err != nil {
		slog.WarnContext(ctx, "codex live models: mint failed", "err", err)
		return nil, nil
	}
	slugs, err := s.Models.ListModels(ctx, token, *row.ChatGPTAccountID)
	if err != nil {
		slog.WarnContext(ctx, "codex live models: fetch failed", "err", err)
		return nil, nil
	}
	out := make([]ModelInfo, 0, len(slugs))
	for _, sl := range slugs {
		out = append(out, ModelInfo{Provider: "openai_codex", ModelID: sl})
	}
	return out, nil
}

// persistConnect seals the token set and upserts the credential + consumes the pending row.
func (s *CodexTokenService) persistConnect(ctx context.Context, pid, bid, jti uuid.UUID, ts codexoauth.TokenSet, p pendingRow) (uuid.UUID, error) {
	if ts.Claims.AccountID == "" {
		return uuid.Nil, fmt.Errorf("codex connect: missing account id: %w", errs.ErrValidation)
	}
	if !chatgptAccountIDRe.MatchString(ts.Claims.AccountID) {
		return uuid.Nil, fmt.Errorf("codex connect: account id has an invalid format: %w", errs.ErrValidation)
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

// codexDB is the DB surface the codex service needs: WithPrincipal (lazy, tenant-scoped) and
// WithTx (the cross-tenant sweep). Satisfied by *db.DB.
type codexDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
	WithTx(ctx context.Context, fn func(pgx.Tx) error) error
}

// NewCodexTokenService assembles a production CodexTokenService (WithPrincipal lazy store +
// WithTx system-sweep store + the auth.openai.com OAuth client). oauth is the codexOAuth seam —
// pass codexoauth.NewClient(...). lazyMargin is cfg.CodexAccessRefreshMargin.
func NewCodexTokenService(db codexDB, sealer *crypto.Sealer, oauth codexOAuth, lazyMargin time.Duration) *CodexTokenService {
	return &CodexTokenService{
		DB:          db,
		Sealer:      sealer,
		OAuth:       oauth,
		Store:       dbCodexStore{DB: db},
		SystemStore: dbSystemCodexStore{DB: db},
		LazyMargin:  lazyMargin,
		Now:         time.Now,
	}
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

func (d dbCodexStore) readCodex(ctx context.Context, pid, bid uuid.UUID) (codexCredRow, error) {
	var out codexCredRow
	err := d.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		r, err := dbgen.New(tx).ReadCodexCredential(ctx, bid)
		if err != nil {
			return err
		}
		out = codexCredRow{
			SealedKeyRef: r.SealedKeyRef, OAuthRefreshToken: r.OauthRefreshToken,
			ChatGPTAccountID: r.ChatgptAccountID, ChatGPTPlan: r.ChatgptPlan,
		}
		if r.OauthAccessExpiry.Valid {
			t := r.OauthAccessExpiry.Time
			out.OAuthAccessExpiry = &t
		}
		return nil
	})
	return out, err
}

func (d dbCodexStore) refreshLockedTx(ctx context.Context, pid, bid uuid.UUID, fn func(codexCredRow) (*upsertTokens, bool, error)) error {
	return d.DB.WithPrincipal(ctx, pid, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		r, err := q.GetCodexCredentialForRefresh(ctx, bid)
		if err != nil {
			return err
		}
		row := codexCredRow{
			SealedKeyRef: r.SealedKeyRef, OAuthRefreshToken: r.OauthRefreshToken,
			ChatGPTAccountID: r.ChatgptAccountID, ChatGPTPlan: r.ChatgptPlan,
		}
		if r.OauthAccessExpiry.Valid {
			t := r.OauthAccessExpiry.Time
			row.OAuthAccessExpiry = &t
		}
		toks, disconnect, ferr := fn(row)
		if ferr != nil {
			return ferr
		}
		if disconnect {
			return q.DisconnectCodexCredential(ctx, bid)
		}
		if toks == nil {
			return nil // fresh; no write
		}
		return q.UpdateCodexOAuthTokens(ctx, dbgen.UpdateCodexOAuthTokensParams{
			SealedKeyRef:      &toks.SealedAccess,
			OauthRefreshToken: &toks.SealedRefresh,
			OauthAccessExpiry: pgtype.Timestamptz{Time: toks.AccessExpiry, Valid: true},
			ChatgptPlan:       &toks.Plan,
			BusinessID:        bid,
		})
	})
}

// dbSystemCodexStore is the production systemCodexStore (Task 6b): raw pgx + the 0096
// SECURITY DEFINER functions, run via WithTx (no principal — cross-tenant by design).
type dbSystemCodexStore struct{ DB codexTxRunner }

func (d dbSystemCodexStore) refreshOneDue(ctx context.Context, cutoff time.Time, exclude []string,
	fn func(claimedCodex) (*upsertTokens, bool, error)) (uuid.UUID, bool, error) {
	var claimedID uuid.UUID
	var handled bool
	err := d.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var c claimedCodex
		var sealedKeyRef *string // returned by claim but unused here (we refresh regardless)
		var plan *string
		row := tx.QueryRow(ctx, `SELECT id, sealed_key_ref, oauth_refresh_token, chatgpt_plan
			FROM codex_claim_for_refresh($1, $2)`, cutoff, exclude)
		if err := row.Scan(&c.ID, &sealedKeyRef, &c.SealedRefresh, &plan); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // nothing due; handled stays false
			}
			return err
		}
		if plan != nil {
			c.Plan = *plan
		}
		claimedID = c.ID
		handled = true
		toks, disconnect, ferr := fn(c)
		if ferr != nil {
			return ferr // rolls back; lock released; credential left for next sweep
		}
		if disconnect {
			_, e := tx.Exec(ctx, `SELECT codex_disconnect_system($1)`, c.ID)
			return e
		}
		_, e := tx.Exec(ctx, `SELECT codex_apply_refresh($1,$2,$3,$4,$5)`,
			c.ID, toks.SealedAccess, toks.SealedRefresh, toks.AccessExpiry, toks.Plan)
		return e
	})
	return claimedID, handled, err
}
