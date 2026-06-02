//go:build integration

package ticketing

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// ---------------------------------------------------------------------------
// T053 — [US4] identity integration RED-GATE.
//
// This file pins the AGREED interface contract (/tmp/mf-us4-interface-contract.md)
// for the sending/receiving IDENTITY surface: custom email domains, their DNS
// verification challenge, custom inbound addresses gated on a verified domain,
// inbound routing of a verified-custom address, and outbound send-identity
// selection.
//
// NEITHER the IdentityService NOR the get_send_identity DEFINER exists yet, so
// this package WILL NOT COMPILE / the get_send_identity SELECT WILL fail. THAT
// IS THE EXPECTED RED (same pattern as triage_integration_test.go's
// "undefined: ...Triage"). T055–T059 implement to match and restore green.
// Because Go can't compile the package while IdentityService is undefined, the
// other ticketing integration tests (triage/read) also won't run during the red
// window — acceptable for this TDD gate.
//
// What is pinned here (no tautologies — every assertion reads REAL state via the
// returned struct, the RLS-exempt Super pool / countSuper, or a raw DEFINER call):
//   - CreateEmailDomain for ALL THREE modes → unverified + populated DNSChallenge
//     (verification TXT `_manyforge.<domain>`=`mf-verify=…`, DKIM TXT
//     `<selector>._domainkey.<domain>` containing `k=ed25519; p=`, MXHint only for
//     subdomain_mx). Persisted row exists. Duplicate→ErrConflict, bad input→ErrValidation.
//   - VerifyEmailDomain (stub resolver): pending poll before publish (unverified, no
//     error); verified after publishing the token; idempotent re-verify; unknown /
//     foreign domain → ErrNotFound (no oracle).
//   - CreateInboundAddress requires a VERIFIED domain (unverified → ErrConflict 409);
//     success → Kind=="custom", row persisted; bad address / dup → ErrValidation/ErrConflict.
//   - Inbound routes to a verified custom address (resolve_inbound_address DEFINER),
//     and an address on an UNVERIFIED domain does NOT resolve (FR-013 drop).
//   - Outbound get_send_identity returns the custom identity when verified, else 0 rows.
//
// FOUND (re: the resolve_inbound_address DEFINER, 0014_support_rls.up.sql:55-65):
// it ALREADY gates custom addresses on `verified_at IS NOT NULL` — a custom
// inbound_address routes only when its email_domain.verified_at is set; system
// addresses (email_domain_id IS NULL) always route. So no DEFINER change is
// needed for inbound routing in this slice; assertion 4 exercises that existing body.
// ---------------------------------------------------------------------------

// b64Sealer is a TEST-ONLY KeySealer stub: Seal base64-encodes the plaintext and
// Open decodes it. It is NOT encryption — it exists only so the red-gate runs
// without depending on T057's AES-256-GCM sealer. Open(Seal(x)) == x, which is the
// only KeySealer property this slice's tests rely on. Replace with the real
// internal/platform/crypto sealer once T057 lands if a confidentiality assertion
// is added.
type b64Sealer struct{}

func (b64Sealer) Seal(plaintext []byte) (string, error) {
	return base64.StdEncoding.EncodeToString(plaintext), nil
}

func (b64Sealer) Open(ref string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(ref)
}

// stubResolver is a deterministic, offline TXTResolver: it returns the records in
// its map for an exact name, and an empty (non-error) result for anything else —
// mirroring net.DefaultResolver returning no records for an unpublished name. The
// test mutates the map to simulate the operator publishing the verification TXT.
type stubResolver struct {
	records map[string][]string
}

func newStubResolver() *stubResolver { return &stubResolver{records: map[string][]string{}} }

func (s *stubResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	return s.records[name], nil
}

// newIdentityService builds the IdentityService under test with the offline stub
// resolver and the test sealer, per the contract:
//
//	&IdentityService{DB: tdb.App, Resolver: <stub>, Sealer: <test sealer>}
func newIdentityService(tdb *testdb.TestDB, resolver *stubResolver) *IdentityService {
	return &IdentityService{DB: tdb.App, Resolver: resolver, Sealer: b64Sealer{}}
}

// uniqueDomain returns a fresh, RFC-ish lowercase domain so tests don't collide on
// the UNIQUE (tenant_root_id, domain) constraint across cases.
func uniqueDomain(label string) string {
	return label + "-" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".example.com"
}

// emailDomainRow reads the raw persisted (verified_at, dkim_selector, mode) for an
// email_domain via the RLS-exempt Super pool — ground truth independent of the service.
func emailDomainRow(ctx context.Context, t *testing.T, tdb *testdb.TestDB, id uuid.UUID) (verifiedAt *time.Time, selector *string, mode string) {
	t.Helper()
	if err := tdb.Super.QueryRow(ctx,
		`SELECT verified_at, dkim_selector, mode::text FROM email_domain WHERE id=$1`,
		id).Scan(&verifiedAt, &selector, &mode); err != nil {
		t.Fatalf("read email_domain row %s: %v", id, err)
	}
	return verifiedAt, selector, mode
}

// seedEmailDomainInTenant inserts an email_domain row DIRECTLY via the Super pool in
// an arbitrary tenant/business (bypassing RLS + the service), returning its id. Used
// to plant a domain owned by a DIFFERENT tenant so the acting principal must get
// ErrNotFound (no existence oracle), identical to a random uuid.
func seedEmailDomainInTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID, tenantRootID uuid.UUID, domain, mode string, verified bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var verifiedExpr string
	if verified {
		verifiedExpr = "now()"
	} else {
		verifiedExpr = "NULL"
	}
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO email_domain (id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,dkim_selector,dkim_public_key,dkim_private_key_ref,spf_state,created_at,updated_at)
		 VALUES ($1,$2,$3,$4,$5::email_domain_mode,$6,`+verifiedExpr+`,'mfseed','cHVi','cmVm','unknown',now(),now())`,
		id, businessID, tenantRootID, domain, mode, "mf-verify=seedtoken"); err != nil {
		t.Fatalf("seed email_domain: %v", err)
	}
	return id
}

// resolveInbound calls the resolve_inbound_address SECURITY DEFINER (0014) the same
// way internal/inbox/resolve.go does — raw QueryRow inside a tx — to assert inbound
// routing. (resolveRecipient/errNoRoute are unexported in package inbox, so this
// reuses the DEFINER directly.) Returns ok=false on zero rows (the silent-drop case).
func resolveInbound(ctx context.Context, t *testing.T, tdb *testdb.TestDB, principal uuid.UUID, address string) (businessID uuid.UUID, emailDomainID *uuid.UUID, ok bool) {
	t.Helper()
	err := tdb.App.WithPrincipal(ctx, principal, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT business_id, email_domain_id FROM resolve_inbound_address($1)`, address,
		).Scan(&businessID, &emailDomainID)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, nil, false
		}
		t.Fatalf("resolve_inbound_address(%q): %v", address, err)
	}
	return businessID, emailDomainID, true
}

// TestIdentityCreateEmailDomainAllModes — CreateEmailDomain for each of the three
// modes returns an unverified domain with a fully-populated DNS challenge, persists
// the row, and rejects duplicates / bad input with the right typed sentinel.
func TestIdentityCreateEmailDomainAllModes(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := newIdentityService(tdb, newStubResolver())

	for _, mode := range []string{"forward_in", "subdomain_mx", "provider_route"} {
		domain := uniqueDomain(mode)
		ed, err := svc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: domain, Mode: mode})
		if err != nil {
			t.Fatalf("CreateEmailDomain(%s): %v", mode, err)
		}

		// Unverified on create.
		if ed.Verification != "unverified" {
			t.Errorf("%s: verification = %q, want unverified", mode, ed.Verification)
		}
		if ed.VerifiedAt != nil {
			t.Errorf("%s: verified_at = %v, want nil on create", mode, ed.VerifiedAt)
		}
		if ed.Mode != mode {
			t.Errorf("%s: returned mode = %q", mode, ed.Mode)
		}

		// Verification TXT challenge.
		wantTXTName := "_manyforge." + domain
		if ed.DNSChallenge.VerificationTXT.Name != wantTXTName {
			t.Errorf("%s: verification TXT name = %q, want %q", mode, ed.DNSChallenge.VerificationTXT.Name, wantTXTName)
		}
		if !strings.HasPrefix(ed.DNSChallenge.VerificationTXT.Value, "mf-verify=") {
			t.Errorf("%s: verification TXT value = %q, want mf-verify= prefix", mode, ed.DNSChallenge.VerificationTXT.Value)
		}

		// DKIM challenge: <selector>._domainkey.<domain>, ed25519 public key.
		if !strings.HasSuffix(ed.DNSChallenge.DKIMRecord.Name, "._domainkey."+domain) {
			t.Errorf("%s: DKIM record name = %q, want <selector>._domainkey.%s", mode, ed.DNSChallenge.DKIMRecord.Name, domain)
		}
		if !strings.Contains(ed.DNSChallenge.DKIMRecord.Value, "k=ed25519; p=") {
			t.Errorf("%s: DKIM record value = %q, want to contain 'k=ed25519; p='", mode, ed.DNSChallenge.DKIMRecord.Value)
		}

		// MXHint set ONLY for subdomain_mx.
		if mode == "subdomain_mx" {
			if ed.DNSChallenge.MXHint == nil || *ed.DNSChallenge.MXHint == "" {
				t.Errorf("%s: MXHint = %v, want non-nil non-empty", mode, ed.DNSChallenge.MXHint)
			}
		} else if ed.DNSChallenge.MXHint != nil {
			t.Errorf("%s: MXHint = %v, want nil (only subdomain_mx gets one)", mode, *ed.DNSChallenge.MXHint)
		}

		// Persisted row exists in the right business/tenant, unverified, with a selector.
		if n := countSuper(ctx, t, tdb.Super,
			`SELECT count(*) FROM email_domain WHERE id=$1 AND business_id=$2 AND tenant_root_id=$3 AND verified_at IS NULL`,
			ed.ID, rt.master, rt.tenantRootID); n != 1 {
			t.Errorf("%s: persisted unverified row count = %d, want 1", mode, n)
		}
		if _, sel, gotMode := emailDomainRow(ctx, t, tdb, ed.ID); gotMode != mode || sel == nil || *sel == "" {
			t.Errorf("%s: persisted mode/selector = %q/%v, want %q + non-empty selector", mode, gotMode, sel, mode)
		}

		// Duplicate (same tenant_root_id, domain) → ErrConflict.
		if _, err := svc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: domain, Mode: mode}); !errors.Is(err, errs.ErrConflict) {
			t.Errorf("%s: duplicate domain: want ErrConflict, got %v", mode, err)
		}
	}

	// Bad domain → ErrValidation; row must not be created.
	if _, err := svc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: "not a domain", Mode: "forward_in"}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("bad domain: want ErrValidation, got %v", err)
	}
	// Bad mode → ErrValidation.
	if _, err := svc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: uniqueDomain("badmode"), Mode: "telepathy"}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("bad mode: want ErrValidation, got %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM email_domain WHERE business_id=$1 AND domain='not a domain'`, rt.master); n != 0 {
		t.Errorf("bad domain wrote %d rows, want 0", n)
	}
}

// TestIdentityVerifyEmailDomain — VerifyEmailDomain polls the (stub) resolver: it is
// a pending no-error poll before the TXT is published, transitions to verified once
// the token is present, is idempotent on a second call, and returns ErrNotFound for
// an unknown/foreign domain (no oracle).
func TestIdentityVerifyEmailDomain(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	resolver := newStubResolver()
	svc := newIdentityService(tdb, resolver)

	domain := uniqueDomain("verify")
	ed, err := svc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: domain, Mode: "forward_in"})
	if err != nil {
		t.Fatalf("CreateEmailDomain: %v", err)
	}
	verifyToken := ed.DNSChallenge.VerificationTXT.Value // the mf-verify=… value to publish

	// --- Before publishing: pending poll → unverified, NO error, verified_at still NULL. ---
	pending, err := svc.VerifyEmailDomain(ctx, rt.owner, rt.master, ed.ID)
	if err != nil {
		t.Fatalf("VerifyEmailDomain (pending): unexpected error %v (a not-yet-observed challenge is a pending poll, not an error)", err)
	}
	if pending.Verification != "unverified" {
		t.Errorf("pending verification = %q, want unverified", pending.Verification)
	}
	if pending.VerifiedAt != nil {
		t.Errorf("pending verified_at = %v, want nil", pending.VerifiedAt)
	}
	if va, _, _ := emailDomainRow(ctx, t, tdb, ed.ID); va != nil {
		t.Errorf("persisted verified_at = %v after pending poll, want NULL", va)
	}

	// --- Publish the token in the stub, then verify → verified, verified_at set. ---
	resolver.records["_manyforge."+domain] = []string{verifyToken}
	verified, err := svc.VerifyEmailDomain(ctx, rt.owner, rt.master, ed.ID)
	if err != nil {
		t.Fatalf("VerifyEmailDomain (verify): %v", err)
	}
	if verified.Verification != "verified" {
		t.Errorf("verification = %q, want verified", verified.Verification)
	}
	if verified.VerifiedAt == nil {
		t.Errorf("verified_at = nil, want non-nil after successful verify")
	}
	va, _, _ := emailDomainRow(ctx, t, tdb, ed.ID)
	if va == nil {
		t.Fatalf("persisted verified_at = NULL after verify, want set")
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM email_domain WHERE id=$1 AND verified_at IS NOT NULL`, ed.ID); n != 1 {
		t.Errorf("verified-row count = %d, want 1", n)
	}

	// --- Idempotent: a second verify returns verified, no error, verified_at UNCHANGED. ---
	again, err := svc.VerifyEmailDomain(ctx, rt.owner, rt.master, ed.ID)
	if err != nil {
		t.Fatalf("VerifyEmailDomain (idempotent): %v", err)
	}
	if again.Verification != "verified" {
		t.Errorf("idempotent verification = %q, want verified", again.Verification)
	}
	if va2, _, _ := emailDomainRow(ctx, t, tdb, ed.ID); va2 == nil || !va2.Equal(*va) {
		t.Errorf("idempotent verify changed verified_at: before=%v after=%v (must be unchanged)", va, va2)
	}

	// --- Unknown vs foreign domain → BOTH ErrNotFound, identical (no oracle). ---
	other := seedReadTenant(ctx, t, tdb)
	foreignID := seedEmailDomainInTenant(ctx, t, tdb, other.master, other.tenantRootID, uniqueDomain("foreign"), "forward_in", false)

	_, errForeign := svc.VerifyEmailDomain(ctx, rt.owner, rt.master, foreignID)
	_, errUnknown := svc.VerifyEmailDomain(ctx, rt.owner, rt.master, uuid.New())
	if !errors.Is(errForeign, errs.ErrNotFound) {
		t.Errorf("foreign-tenant domain: want ErrNotFound, got %v", errForeign)
	}
	if !errors.Is(errUnknown, errs.ErrNotFound) {
		t.Errorf("unknown domain: want ErrNotFound, got %v", errUnknown)
	}
	if (errForeign == nil) != (errUnknown == nil) || errForeign.Error() != errUnknown.Error() {
		t.Errorf("verify oracle: foreign (%v) and unknown (%v) must be identical", errForeign, errUnknown)
	}
	// The foreign domain must NOT have been verified by our acting principal.
	if va, _, _ := emailDomainRow(ctx, t, tdb, foreignID); va != nil {
		t.Errorf("foreign domain verified_at = %v, want NULL (cross-tenant verify must not mutate)", va)
	}
}

// TestIdentityCreateInboundAddressRequiresVerifiedDomain — a custom inbound address
// can only be created against a VERIFIED, owned domain. Unverified → ErrConflict (409);
// verified → success (Kind=custom, EmailDomainID set, row persisted). Address not on
// the domain → ErrValidation; duplicate → ErrConflict.
func TestIdentityCreateInboundAddressRequiresVerifiedDomain(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	resolver := newStubResolver()
	svc := newIdentityService(tdb, resolver)

	domain := uniqueDomain("inbound")
	ed, err := svc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: domain, Mode: "subdomain_mx"})
	if err != nil {
		t.Fatalf("CreateEmailDomain: %v", err)
	}

	// --- Against an UNVERIFIED domain → ErrConflict (409), no row written. ---
	addr := "support@" + domain
	if _, err := svc.CreateInboundAddress(ctx, rt.owner, rt.master, CreateInboundAddressInput{Address: addr, EmailDomainID: ed.ID}); !errors.Is(err, errs.ErrConflict) {
		t.Errorf("unverified domain inbound address: want ErrConflict, got %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM inbound_address WHERE address=$1`, addr); n != 0 {
		t.Errorf("unverified-domain inbound address wrote %d rows, want 0", n)
	}

	// --- Verify the domain (publish + verify), then create succeeds. ---
	resolver.records["_manyforge."+domain] = []string{ed.DNSChallenge.VerificationTXT.Value}
	if _, err := svc.VerifyEmailDomain(ctx, rt.owner, rt.master, ed.ID); err != nil {
		t.Fatalf("VerifyEmailDomain: %v", err)
	}

	ia, err := svc.CreateInboundAddress(ctx, rt.owner, rt.master, CreateInboundAddressInput{Address: addr, EmailDomainID: ed.ID})
	if err != nil {
		t.Fatalf("CreateInboundAddress (verified): %v", err)
	}
	if ia.Kind != "custom" {
		t.Errorf("kind = %q, want custom", ia.Kind)
	}
	if ia.EmailDomainID == nil || *ia.EmailDomainID != ed.ID {
		t.Errorf("email_domain_id = %v, want %v", ia.EmailDomainID, ed.ID)
	}
	if n := countSuper(ctx, t, tdb.Super,
		`SELECT count(*) FROM inbound_address WHERE id=$1 AND address=$2 AND kind='custom' AND email_domain_id=$3 AND business_id=$4`,
		ia.ID, addr, ed.ID, rt.master); n != 1 {
		t.Errorf("persisted custom inbound_address count = %d, want 1", n)
	}

	// --- Address not ending in @<domain> → ErrValidation. ---
	if _, err := svc.CreateInboundAddress(ctx, rt.owner, rt.master, CreateInboundAddressInput{Address: "help@other-domain.example.com", EmailDomainID: ed.ID}); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("address not on domain: want ErrValidation, got %v", err)
	}

	// --- Duplicate address → ErrConflict. ---
	if _, err := svc.CreateInboundAddress(ctx, rt.owner, rt.master, CreateInboundAddressInput{Address: addr, EmailDomainID: ed.ID}); !errors.Is(err, errs.ErrConflict) {
		t.Errorf("duplicate address: want ErrConflict, got %v", err)
	}
}

// TestIdentityInboundRoutesToCustomAddress — once a domain is verified and a custom
// inbound address is created, resolve_inbound_address routes that address to the
// right business + the custom email_domain_id. An address on an UNVERIFIED domain
// does NOT resolve (zero rows → FR-013 silent inbound drop). Asserts the existing
// 0014 DEFINER's verified_at gate end-to-end.
func TestIdentityInboundRoutesToCustomAddress(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	resolver := newStubResolver()
	svc := newIdentityService(tdb, resolver)

	// Verified domain + custom address.
	verifiedDomain := uniqueDomain("route-ok")
	edOK, err := svc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: verifiedDomain, Mode: "subdomain_mx"})
	if err != nil {
		t.Fatalf("CreateEmailDomain (ok): %v", err)
	}
	resolver.records["_manyforge."+verifiedDomain] = []string{edOK.DNSChallenge.VerificationTXT.Value}
	if _, err := svc.VerifyEmailDomain(ctx, rt.owner, rt.master, edOK.ID); err != nil {
		t.Fatalf("VerifyEmailDomain (ok): %v", err)
	}
	okAddr := "support@" + verifiedDomain
	if _, err := svc.CreateInboundAddress(ctx, rt.owner, rt.master, CreateInboundAddressInput{Address: okAddr, EmailDomainID: edOK.ID}); err != nil {
		t.Fatalf("CreateInboundAddress (ok): %v", err)
	}

	// Resolves to the right business + custom email_domain_id.
	gotBiz, gotDomain, ok := resolveInbound(ctx, t, tdb, rt.owner, okAddr)
	if !ok {
		t.Fatalf("verified custom address %q did not resolve, want a route", okAddr)
	}
	if gotBiz != rt.master {
		t.Errorf("routed business = %s, want %s", gotBiz, rt.master)
	}
	if gotDomain == nil || *gotDomain != edOK.ID {
		t.Errorf("routed email_domain_id = %v, want %v", gotDomain, edOK.ID)
	}

	// --- An address on an UNVERIFIED domain must NOT resolve (FR-013 drop). ---
	// Plant an UNVERIFIED domain + a custom address pointed at it directly via the
	// Super pool (the service refuses to create a custom address on an unverified
	// domain — that's TestIdentityCreateInboundAddressRequiresVerifiedDomain — so we
	// seed the row to prove the DEFINER itself drops it).
	unverifiedDomain := uniqueDomain("route-drop")
	edDrop := seedEmailDomainInTenant(ctx, t, tdb, rt.master, rt.tenantRootID, unverifiedDomain, "subdomain_mx", false)
	dropAddr := "support@" + unverifiedDomain
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at)
		 VALUES ($1,$2,$3,$4,'custom',$5,now(),now())`,
		uuid.New(), rt.master, rt.tenantRootID, dropAddr, edDrop); err != nil {
		t.Fatalf("seed unverified custom inbound_address: %v", err)
	}
	if _, _, ok := resolveInbound(ctx, t, tdb, rt.owner, dropAddr); ok {
		t.Errorf("address on unverified domain resolved, want zero rows (FR-013 silent inbound drop)")
	}
}

// TestIdentityOutboundSendIdentitySelection — the get_send_identity DEFINER returns
// the custom from_address + dkim fields when the business has a verified domain with
// a custom inbound address, and ZERO rows when it does not (caller falls back to the
// system identity). The get_send_identity DEFINER does not exist yet → EXPECTED RED.
func TestIdentityOutboundSendIdentitySelection(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	resolver := newStubResolver()
	svc := newIdentityService(tdb, resolver)

	getSendIdentity := func(businessID, tenantRootID uuid.UUID) (fromAddr, dkimDomain, dkimSelector, dkimRef *string, rows int) {
		t.Helper()
		err := tdb.App.WithPrincipal(ctx, rt.owner, func(tx pgx.Tx) error {
			r, err := tx.Query(ctx,
				`SELECT from_address, dkim_domain, dkim_selector, dkim_private_key_ref FROM get_send_identity($1,$2)`,
				businessID, tenantRootID)
			if err != nil {
				return err
			}
			defer r.Close()
			for r.Next() {
				rows++
				if err := r.Scan(&fromAddr, &dkimDomain, &dkimSelector, &dkimRef); err != nil {
					return err
				}
			}
			return r.Err()
		})
		if err != nil {
			t.Fatalf("get_send_identity(%s,%s): %v", businessID, tenantRootID, err)
		}
		return fromAddr, dkimDomain, dkimSelector, dkimRef, rows
	}

	// --- No verified domain yet → 0 rows (system fallback). ---
	if _, _, _, _, rows := getSendIdentity(rt.master, rt.tenantRootID); rows != 0 {
		t.Errorf("get_send_identity with no verified domain returned %d rows, want 0 (system fallback)", rows)
	}

	// --- Verified domain + custom address → 1 row with custom from_address + dkim fields. ---
	domain := uniqueDomain("send")
	ed, err := svc.CreateEmailDomain(ctx, rt.owner, rt.master, CreateEmailDomainInput{Domain: domain, Mode: "provider_route"})
	if err != nil {
		t.Fatalf("CreateEmailDomain: %v", err)
	}
	resolver.records["_manyforge."+domain] = []string{ed.DNSChallenge.VerificationTXT.Value}
	if _, err := svc.VerifyEmailDomain(ctx, rt.owner, rt.master, ed.ID); err != nil {
		t.Fatalf("VerifyEmailDomain: %v", err)
	}
	customAddr := "support@" + domain
	if _, err := svc.CreateInboundAddress(ctx, rt.owner, rt.master, CreateInboundAddressInput{Address: customAddr, EmailDomainID: ed.ID}); err != nil {
		t.Fatalf("CreateInboundAddress: %v", err)
	}

	fromAddr, dkimDomain, dkimSelector, dkimRef, rows := getSendIdentity(rt.master, rt.tenantRootID)
	if rows != 1 {
		t.Fatalf("get_send_identity with verified custom domain returned %d rows, want 1", rows)
	}
	if fromAddr == nil || *fromAddr != customAddr {
		t.Errorf("from_address = %v, want %q", fromAddr, customAddr)
	}
	if dkimDomain == nil || !strings.EqualFold(*dkimDomain, domain) {
		t.Errorf("dkim_domain = %v, want %q", dkimDomain, domain)
	}
	if dkimSelector == nil || *dkimSelector == "" {
		t.Errorf("dkim_selector = %v, want non-empty", dkimSelector)
	}
	if dkimRef == nil || *dkimRef == "" {
		t.Errorf("dkim_private_key_ref = %v, want non-empty", dkimRef)
	}
}
