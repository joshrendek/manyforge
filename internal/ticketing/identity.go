package ticketing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// TXTResolver looks up DNS TXT records for the email-domain verification poll
// (T056). Production wraps net.DefaultResolver with a context timeout
// (NetTXTResolver, below); tests inject a deterministic in-memory stub. It is
// HTTP-free on purpose — netsafe guards outbound HTTP, not DNS.
type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// KeySealer encrypts/decrypts secrets at rest (the per-domain DKIM Ed25519 private
// key). Production is internal/platform/crypto.Sealer (AES-256-GCM); tests inject a
// stub. The returned ref is an opaque string stored in
// email_domain.dkim_private_key_ref — NEVER the raw key, NEVER logged.
type KeySealer interface {
	Seal(plaintext []byte) (ref string, err error)
	Open(ref string) (plaintext []byte, err error)
}

// IdentityService owns the US4 inbox-management surface: custom email domains, their
// DNS-verification lifecycle, the per-domain DKIM keypair generated at create time,
// and custom inbound addresses bound to a verified domain. Every method takes the
// caller's principalID + the target businessID and runs inside db.WithPrincipal
// (RLS scopes rows to the caller's authorized businesses) while ALSO pushing the
// ownership predicate (business_id = $) into SQL — dual enforcement. Unknown /
// other-business / unauthorized all collapse to ErrNotFound (404, no existence
// oracle). Each mutation writes an audit.Entry in the SAME transaction (FR-014).
type IdentityService struct {
	DB       *db.DB
	Resolver TXTResolver
	Sealer   KeySealer

	// SystemMailHost is the platform mail host advised in the SPF/MX hints. Empty ⇒
	// the package default (a sensible advisory constant); these hints are advisory
	// and never machine-read, so a default is safe.
	SystemMailHost string

	// Optional determinism hook; defaults to the real implementation when nil
	// (the integration tests construct the service WITHOUT setting it).
	Rand io.Reader // default crypto/rand.Reader (verify token + DKIM keygen)
}

// CreateEmailDomainInput is the validated POST email-domains payload.
type CreateEmailDomainInput struct {
	Domain string
	Mode   string // one of forward_in | subdomain_mx | provider_route
}

// CreateInboundAddressInput is the validated POST inbound-addresses payload.
type CreateInboundAddressInput struct {
	Address       string
	EmailDomainID uuid.UUID
}

// Pagination is a keyset page request. Limit defaults to 50 and is capped at 100.
type Pagination struct {
	Cursor string
	Limit  int
}

// EmailDomain is the API view of a custom domain / sending identity. JSON field
// names match the openapi.yaml EmailDomain schema exactly.
type EmailDomain struct {
	ID           uuid.UUID    `json:"id"`
	BusinessID   uuid.UUID    `json:"business_id"`
	TenantRootID uuid.UUID    `json:"tenant_root_id"`
	Domain       string       `json:"domain"`
	Mode         string       `json:"mode"`
	Verification string       `json:"verification"` // derived: verified_at==nil → "unverified" else "verified"
	VerifiedAt   *time.Time   `json:"verified_at"`
	DKIMState    string       `json:"dkim_state"` // unknown|pending|pass|fail
	SPFState     string       `json:"spf_state"`
	DNSChallenge DNSChallenge `json:"dns_challenge"`
	CreatedAt    time.Time    `json:"created_at"`
}

// DNSChallenge is the set of records an operator publishes to prove ownership and
// enable outbound auth (FR-012/FR-013).
type DNSChallenge struct {
	VerificationTXT TXTRecord `json:"verification_txt"`
	DKIMRecord      TXTRecord `json:"dkim_record"`
	SPFHint         string    `json:"spf_hint"`
	MXHint          *string   `json:"mx_hint"` // set only for subdomain_mx; nil otherwise
}

// TXTRecord is a single publishable DNS record {name, value}.
type TXTRecord struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// InboundAddress is the API view of a routing entry. JSON field names match the
// openapi.yaml InboundAddress schema exactly.
type InboundAddress struct {
	ID            uuid.UUID  `json:"id"`
	BusinessID    uuid.UUID  `json:"business_id"`
	TenantRootID  uuid.UUID  `json:"tenant_root_id"`
	Address       string     `json:"address"`
	Kind          string     `json:"kind"` // system|custom
	EmailDomainID *uuid.UUID `json:"email_domain_id"`
	Active        bool       `json:"active"` // always true in this slice
	CreatedAt     time.Time  `json:"created_at"`
}

// domainRe is the RFC-valid lowercase domain shape accepted at create time. Labels
// are lowercase letters/digits/hyphens only (underscores are NOT valid in DNS host
// labels per RFC 952/1123), dot-separated, with a 2+ letter TLD.
// Junk with a space ("not a domain") or no dot is a clean 400, never an insert.
var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*\.[a-z]{2,}$`)

const defaultSystemMailHost = "mail.manyforge.example"

// rand returns the service entropy source (s.Rand or crypto/rand.Reader).
func (s *IdentityService) rand() io.Reader {
	if s.Rand != nil {
		return s.Rand
	}
	return rand.Reader
}

func (s *IdentityService) systemMailHost() string {
	if s.SystemMailHost != "" {
		return s.SystemMailHost
	}
	return defaultSystemMailHost
}

func isValidMode(m string) bool {
	switch m {
	case "forward_in", "subdomain_mx", "provider_route":
		return true
	}
	return false
}

// CreateEmailDomain creates a custom email domain (T055) and its DKIM keypair (T057
// keygen). It validates the domain + mode, generates a verify token and an Ed25519
// keypair, seals the private key, INSERTs the row (unverified), writes the
// email_domain.created audit in the same tx, and returns the domain with its full
// DNS challenge. The private key is sealed via s.Sealer; a nil Sealer is a hard
// (non-sentinel) error so the feature never persists an unsealed key.
func (s *IdentityService) CreateEmailDomain(ctx context.Context, principalID, businessID uuid.UUID, in CreateEmailDomainInput) (EmailDomain, error) {
	domain := strings.ToLower(strings.TrimSpace(in.Domain))
	if !domainRe.MatchString(domain) {
		return EmailDomain{}, fmt.Errorf("ticketing: invalid domain %q: %w", in.Domain, errs.ErrValidation)
	}
	if !isValidMode(in.Mode) {
		return EmailDomain{}, fmt.Errorf("ticketing: invalid mode %q: %w", in.Mode, errs.ErrValidation)
	}
	if s.Sealer == nil {
		// security: the feature requires the at-rest master key. Refuse rather than
		// store an unsealed private key. Non-sentinel ⇒ generic 500.
		return EmailDomain{}, errors.New("ticketing: DKIM sealer not configured")
	}

	// Verify token: mf-verify=<base64url(32 random bytes)>.
	tokenBytes := make([]byte, 32)
	if _, err := io.ReadFull(s.rand(), tokenBytes); err != nil {
		return EmailDomain{}, fmt.Errorf("ticketing: generate verify token: %w", err)
	}
	verifyToken := "mf-verify=" + base64.RawURLEncoding.EncodeToString(tokenBytes)

	// Per-domain Ed25519 DKIM keypair. The public key is stored BARE (the p= payload
	// only); the full TXT record value is composed at challenge-build time. The
	// private key is sealed before it ever touches a column.
	pub, priv, err := ed25519.GenerateKey(s.rand())
	if err != nil {
		return EmailDomain{}, fmt.Errorf("ticketing: generate DKIM key: %w", err)
	}
	pubKeyB64 := base64.StdEncoding.EncodeToString(pub)
	selector, err := s.dkimSelector()
	if err != nil {
		return EmailDomain{}, fmt.Errorf("ticketing: generate selector: %w", err)
	}
	sealedRef, err := s.Sealer.Seal(priv)
	if err != nil {
		// Do not wrap: the underlying error never carries secret bytes, but keep the
		// surface generic (500) — sealing is an internal failure, not caller input.
		return EmailDomain{}, errors.New("ticketing: seal DKIM key")
	}

	domainID := uuid.New()
	var row dbgen.EmailDomain
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		r, ierr := q.InsertEmailDomain(ctx, dbgen.InsertEmailDomainParams{
			ID:                domainID,
			BusinessID:        businessID,
			Domain:            domain,
			Mode:              dbgen.EmailDomainMode(in.Mode),
			VerifyToken:       verifyToken,
			DkimSelector:      &selector,
			DkimPublicKey:     &pubKeyB64,
			DkimPrivateKeyRef: &sealedRef,
		})
		if ierr != nil {
			return ierr
		}
		row = r

		targetType := "email_domain"
		return audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			TenantRootID:     &row.TenantRootID,
			ActorPrincipalID: &principalID,
			Action:           "email_domain.created",
			TargetType:       &targetType,
			TargetID:         &row.ID,
			NewValue:         map[string]any{"domain": domain, "mode": in.Mode},
		})
	})
	if err != nil {
		return EmailDomain{}, mapIdentityErr(err)
	}
	return s.toEmailDomain(row, "pending"), nil
}

// dkimSelector builds a per-domain selector "mf" + 8 hex chars (4 random bytes).
func (s *IdentityService) dkimSelector() (string, error) {
	b := make([]byte, 4)
	if _, err := io.ReadFull(s.rand(), b); err != nil {
		return "", err
	}
	return "mf" + hex.EncodeToString(b), nil
}

// ListEmailDomains returns a keyset page of a business's email domains, oldest
// first. The DNS challenge is recomposed from the stored token + selector + public
// key for each row so the operator can re-read the records to publish.
func (s *IdentityService) ListEmailDomains(ctx context.Context, principalID, businessID uuid.UUID, p Pagination) ([]EmailDomain, string, error) {
	lim := clampLimit(p.Limit)
	var out []EmailDomain
	var nextCursor string
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var rows []dbgen.EmailDomain
		var qerr error
		if p.Cursor == "" {
			rows, qerr = q.ListEmailDomains(ctx, dbgen.ListEmailDomainsParams{
				BusinessID: businessID, Lim: int32(lim + 1),
			})
		} else {
			k, derr := decodeEmailDomainCursor(p.Cursor)
			if derr != nil {
				return derr
			}
			rows, qerr = q.ListEmailDomainsAfter(ctx, dbgen.ListEmailDomainsAfterParams{
				BusinessID: businessID, CurCreatedAt: k.ts, CurID: k.id, Lim: int32(lim + 1),
			})
		}
		if qerr != nil {
			return qerr
		}
		rows, next := trim(rows, lim)
		out = make([]EmailDomain, 0, len(rows))
		for _, r := range rows {
			out = append(out, s.toEmailDomain(r, s.dkimStateForList(r)))
		}
		if next {
			last := rows[len(rows)-1]
			nextCursor = encodeEmailDomainCursor(keyset{ts: last.CreatedAt, id: last.ID})
		}
		return nil
	})
	if err != nil {
		return nil, "", mapIdentityErr(err)
	}
	return out, nextCursor, nil
}

// dkimStateForList projects a persisted-only DKIM state for a list view: a verified
// domain is reported "pass" (its identity is in use), an unverified one "pending".
// (Active DKIM probing happens in VerifyEmailDomain, not on every list read.)
func (s *IdentityService) dkimStateForList(r dbgen.EmailDomain) string {
	if r.VerifiedAt.Valid {
		return "pass"
	}
	return "pending"
}

// VerifyEmailDomain polls the DNS TXT records to verify a domain (T056). It loads
// the owned domain (unknown/foreign → ErrNotFound, no oracle), looks up
// _manyforge.<domain>; if the stored verify token is published it sets verified_at
// (only when currently NULL — idempotent) and audits the transition. It also probes
// <selector>._domainkey.<domain> to report DKIM pass/pending. A not-yet-observed
// challenge is a pending poll: the domain is returned unverified with NO error.
func (s *IdentityService) VerifyEmailDomain(ctx context.Context, principalID, businessID, domainID uuid.UUID) (EmailDomain, error) {
	var out EmailDomain
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, gerr := q.GetEmailDomain(ctx, dbgen.GetEmailDomainParams{ID: domainID, BusinessID: businessID})
		if gerr != nil {
			return gerr
		}

		// DKIM probe: does the published _domainkey TXT contain our public key?
		dkimState := "pending"
		if row.DkimSelector != nil && row.DkimPublicKey != nil {
			dkimName := *row.DkimSelector + "._domainkey." + row.Domain
			if recs, _ := s.Resolver.LookupTXT(ctx, dkimName); containsSubstr(recs, *row.DkimPublicKey) {
				dkimState = "pass"
			}
		}

		// Ownership TXT poll.
		published := false
		if recs, _ := s.Resolver.LookupTXT(ctx, "_manyforge."+row.Domain); containsExact(recs, row.VerifyToken) {
			published = true
		}

		alreadyVerified := row.VerifiedAt.Valid
		if published && !alreadyVerified {
			if uerr := q.MarkEmailDomainVerified(ctx, dbgen.MarkEmailDomainVerifiedParams{
				ID: row.ID, BusinessID: businessID, TenantRootID: row.TenantRootID,
			}); uerr != nil {
				return uerr
			}
			// Re-read so the returned verified_at reflects the committed timestamp.
			updated, rerr := q.GetEmailDomain(ctx, dbgen.GetEmailDomainParams{ID: domainID, BusinessID: businessID})
			if rerr != nil {
				return rerr
			}
			row = updated

			targetType := "email_domain"
			if aerr := audit.Write(ctx, tx, audit.Entry{
				BusinessID:       &businessID,
				TenantRootID:     &row.TenantRootID,
				ActorPrincipalID: &principalID,
				Action:           "email_domain.verified",
				TargetType:       &targetType,
				TargetID:         &row.ID,
				NewValue:         map[string]any{"domain": row.Domain},
			}); aerr != nil {
				return aerr
			}
		}

		out = s.toEmailDomain(row, dkimState)
		return nil
	})
	if err != nil {
		return EmailDomain{}, mapIdentityErr(err)
	}
	return out, nil
}

// CreateInboundAddress creates a custom inbound address bound to a verified, owned
// domain (T058). It validates the address (must end in @<domain>), requires the
// referenced domain be owned (unknown/foreign → 404) AND verified (owned-but-
// unverified → 409, enforced in SQL), INSERTs the kind='custom' row, and audits in
// the same tx. A duplicate address → 409.
func (s *IdentityService) CreateInboundAddress(ctx context.Context, principalID, businessID uuid.UUID, in CreateInboundAddressInput) (InboundAddress, error) {
	address := strings.ToLower(strings.TrimSpace(in.Address))
	if !looksLikeEmail(address) {
		return InboundAddress{}, fmt.Errorf("ticketing: invalid address: %w", errs.ErrValidation)
	}

	var out InboundAddress
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)

		// Load the referenced domain (own-checked). Unknown/foreign → ErrNoRows →
		// ErrNotFound (no oracle). We need its domain string to validate the address
		// shape and its verified_at to distinguish 409 (unverified) from a successful
		// insert. The INSERT itself ALSO re-checks ownership + verified_at in SQL.
		ed, gerr := q.GetEmailDomain(ctx, dbgen.GetEmailDomainParams{ID: in.EmailDomainID, BusinessID: businessID})
		if gerr != nil {
			return gerr
		}

		// Address must be on this domain.
		if !strings.HasSuffix(address, "@"+strings.ToLower(ed.Domain)) {
			return fmt.Errorf("ticketing: address not on domain: %w", errs.ErrValidation)
		}

		// The domain must be verified (FR-012). Owned-but-unverified → 409.
		if !ed.VerifiedAt.Valid {
			return fmt.Errorf("ticketing: domain not verified: %w", errs.ErrConflict)
		}

		row, ierr := q.InsertCustomInboundAddress(ctx, dbgen.InsertCustomInboundAddressParams{
			ID:            uuid.New(),
			Address:       address,
			EmailDomainID: in.EmailDomainID,
			BusinessID:    businessID,
		})
		if ierr != nil {
			return ierr
		}

		targetType := "inbound_address"
		if aerr := audit.Write(ctx, tx, audit.Entry{
			BusinessID:       &businessID,
			TenantRootID:     &row.TenantRootID,
			ActorPrincipalID: &principalID,
			Action:           "inbound_address.created",
			TargetType:       &targetType,
			TargetID:         &row.ID,
			NewValue:         map[string]any{"address": address, "kind": "custom", "email_domain_id": in.EmailDomainID},
		}); aerr != nil {
			return aerr
		}

		out = toInboundAddress(row)
		return nil
	})
	if err != nil {
		return InboundAddress{}, mapIdentityErr(err)
	}
	return out, nil
}

// ListInboundAddresses returns a keyset page of a business's inbound addresses
// (system and custom), oldest first.
func (s *IdentityService) ListInboundAddresses(ctx context.Context, principalID, businessID uuid.UUID, p Pagination) ([]InboundAddress, string, error) {
	lim := clampLimit(p.Limit)
	var out []InboundAddress
	var nextCursor string
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var rows []dbgen.InboundAddress
		var qerr error
		if p.Cursor == "" {
			rows, qerr = q.ListInboundAddresses(ctx, dbgen.ListInboundAddressesParams{
				BusinessID: businessID, Lim: int32(lim + 1),
			})
		} else {
			k, derr := decodeInboundAddressCursor(p.Cursor)
			if derr != nil {
				return derr
			}
			rows, qerr = q.ListInboundAddressesAfter(ctx, dbgen.ListInboundAddressesAfterParams{
				BusinessID: businessID, CurCreatedAt: k.ts, CurID: k.id, Lim: int32(lim + 1),
			})
		}
		if qerr != nil {
			return qerr
		}
		rows, next := trim(rows, lim)
		out = make([]InboundAddress, 0, len(rows))
		for _, r := range rows {
			out = append(out, toInboundAddress(r))
		}
		if next {
			last := rows[len(rows)-1]
			nextCursor = encodeInboundAddressCursor(keyset{ts: last.CreatedAt, id: last.ID})
		}
		return nil
	})
	if err != nil {
		return nil, "", mapIdentityErr(err)
	}
	return out, nextCursor, nil
}

// --- projection helpers ---

// toEmailDomain projects a persisted row into the API view, recomposing the full DNS
// challenge (the DKIM TXT value v=DKIM1; k=ed25519; p=<bare base64> is composed
// here, never stored). dkimState is the caller-supplied projected state.
func (s *IdentityService) toEmailDomain(r dbgen.EmailDomain, dkimState string) EmailDomain {
	verification := "unverified"
	var verifiedAt *time.Time
	if r.VerifiedAt.Valid {
		verification = "verified"
		t := r.VerifiedAt.Time
		verifiedAt = &t
	}

	selector := ""
	if r.DkimSelector != nil {
		selector = *r.DkimSelector
	}
	pubKey := ""
	if r.DkimPublicKey != nil {
		pubKey = *r.DkimPublicKey
	}

	var mxHint *string
	if r.Mode == dbgen.EmailDomainModeSubdomainMx {
		h := s.systemMailHost()
		mxHint = &h
	}

	return EmailDomain{
		ID:           r.ID,
		BusinessID:   r.BusinessID,
		TenantRootID: r.TenantRootID,
		Domain:       r.Domain,
		Mode:         string(r.Mode),
		Verification: verification,
		VerifiedAt:   verifiedAt,
		DKIMState:    dkimState,
		SPFState:     string(r.SpfState),
		DNSChallenge: DNSChallenge{
			VerificationTXT: TXTRecord{Name: "_manyforge." + r.Domain, Value: r.VerifyToken},
			DKIMRecord:      TXTRecord{Name: selector + "._domainkey." + r.Domain, Value: "v=DKIM1; k=ed25519; p=" + pubKey},
			SPFHint:         "v=spf1 include:" + s.systemMailHost() + " ~all",
			MXHint:          mxHint,
		},
		CreatedAt: r.CreatedAt,
	}
}

func toInboundAddress(r dbgen.InboundAddress) InboundAddress {
	return InboundAddress{
		ID:            r.ID,
		BusinessID:    r.BusinessID,
		TenantRootID:  r.TenantRootID,
		Address:       r.Address,
		Kind:          string(r.Kind),
		EmailDomainID: pgUUIDPtr(r.EmailDomainID),
		Active:        true,
		CreatedAt:     r.CreatedAt,
	}
}

// --- small helpers ---

// looksLikeEmail is a deliberately-loose local-part@domain check; the authoritative
// "address is on the verified domain" check is the @<domain> suffix match in
// CreateInboundAddress.
func looksLikeEmail(addr string) bool {
	at := strings.IndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		return false
	}
	if strings.Count(addr, "@") != 1 {
		return false
	}
	return domainRe.MatchString(addr[at+1:])
}

func containsExact(records []string, want string) bool {
	for _, r := range records {
		if r == want {
			return true
		}
	}
	return false
}

func containsSubstr(records []string, want string) bool {
	for _, r := range records {
		if strings.Contains(r, want) {
			return true
		}
	}
	return false
}

// mapIdentityErr maps a query/closure error to a stable service-layer sentinel.
// pgx.ErrNoRows (a single-row lookup miss) → ErrNotFound (no oracle). A Postgres
// unique violation (23505) → ErrConflict (duplicate domain/address). Existing
// typed sentinels pass through. Everything else is wrapped (server-side log → 500).
func mapIdentityErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("ticketing: not found: %w", errs.ErrNotFound)
	case isUniqueViolation(err):
		return fmt.Errorf("ticketing: duplicate: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("ticketing: query: %w", err)
	}
}

// isUniqueViolation reports whether err is a Postgres unique-constraint violation
// (SQLSTATE 23505) — e.g. a duplicate (tenant_root_id, domain) on email_domain or
// (tenant_root_id, address) on inbound_address. The caller maps it to ErrConflict.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// --- production TXT resolver (T056) ---

// NetTXTResolver is the production TXTResolver: it wraps net.DefaultResolver with a
// per-lookup timeout so a slow/missing zone cannot stall a verify request. Exported
// so cmd/manyforge can inject it.
type NetTXTResolver struct {
	// Timeout bounds a single LookupTXT; <=0 uses DefaultTXTLookupTimeout.
	Timeout time.Duration
}

// DefaultTXTLookupTimeout caps a single TXT lookup.
const DefaultTXTLookupTimeout = 5 * time.Second

// LookupTXT resolves the TXT records for name with a bounded context. A lookup that
// finds no records returns an empty slice (mirroring the stub in tests); a
// genuine resolver error is returned to the caller, which treats it as "not yet
// observed" (a pending poll), never a hard failure.
func (n NetTXTResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	d := n.Timeout
	if d <= 0 {
		d = DefaultTXTLookupTimeout
	}
	lctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return net.DefaultResolver.LookupTXT(lctx, name)
}
