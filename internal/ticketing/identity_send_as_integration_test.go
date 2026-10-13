//go:build integration

package ticketing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// Spec 017 addendum: a custom inbound address's send_as override. The domain rule
// (same business's verified Resend profile from_email domain) lives in the UPDATE,
// and get_resend_support_identity returns send_as as from_address when set.

// seedSendAsFixture gives rt a verified provider_route domain support.<apex>, a
// custom support@support.<apex> address, and a resend profile From support@<apex>
// in profileStatus. It returns the apex and the custom address id.
func seedSendAsFixture(ctx context.Context, t *testing.T, tdb *testdb.TestDB, rt readTenant, profileStatus string) (apex string, addressID uuid.UUID) {
	t.Helper()
	apex = "sendas-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] + ".example.com"
	domainID := seedEmailDomainInTenant(ctx, t, tdb, rt.master, rt.tenantRootID, "support."+apex, "provider_route", true)
	addressID, secretID := uuid.New(), uuid.New()
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at)
		  VALUES ($1,$2,$3,$4,'custom',$5,now(),now())`, []any{addressID, rt.master, rt.tenantRootID, "support@support." + apex, domainID}},
		{`INSERT INTO secret (id,business_id,tenant_root_id,scope,sealed_value,created_at,updated_at)
		  VALUES ($1,$2,$3,'mailing','sealed',now(),now())`, []any{secretID, rt.master, rt.tenantRootID}},
		{`INSERT INTO mailing_sending_profile (id,business_id,tenant_root_id,mode,from_email,from_name,secret_ref,status)
		  VALUES ($1,$2,$3,'resend',$4,'Support',$5,$6)`, []any{uuid.New(), rt.master, rt.tenantRootID, "support@" + apex, secretID, profileStatus}},
	} {
		if _, err := tdb.Super.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed send-as fixture: %v\nSQL: %s", err, s.sql)
		}
	}
	return apex, addressID
}

func strp(s string) *string { return &s }

// resendIdentityFrom reads (from_address, inbound_address) through the DEFINER as the app role.
func resendIdentityFrom(ctx context.Context, t *testing.T, tdb *testdb.TestDB, rt readTenant) (from, inbound string) {
	t.Helper()
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT from_address, inbound_address FROM get_resend_support_identity($1, $2)`,
			rt.master, rt.tenantRootID).Scan(&from, &inbound)
	}); err != nil {
		t.Fatalf("get_resend_support_identity: %v", err)
	}
	return from, inbound
}

func TestIdentitySetInboundAddressSendAs(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := newIdentityService(tdb, newStubResolver())
	apex, addrID := seedSendAsFixture(ctx, t, tdb, rt, "verified")
	inboundAddr := "support@support." + apex

	// Unset: the DEFINER sends From the inbound address itself.
	if from, inbound := resendIdentityFrom(ctx, t, tdb, rt); from != inboundAddr || inbound != inboundAddr {
		t.Fatalf("identity before send_as = %q / %q, want %q for both", from, inbound, inboundAddr)
	}

	// Set (trimmed + lowercased), on the profile's domain.
	got, err := svc.SetInboundAddressSendAs(ctx, rt.owner, rt.master, addrID, strp("  Support@"+strings.ToUpper(apex)+" "))
	if err != nil {
		t.Fatalf("SetInboundAddressSendAs: %v", err)
	}
	want := "support@" + apex
	if got.SendAs == nil || *got.SendAs != want || got.Address != inboundAddr {
		t.Fatalf("returned send_as=%v address=%q, want %q / %q", got.SendAs, got.Address, want, inboundAddr)
	}
	if from, inbound := resendIdentityFrom(ctx, t, tdb, rt); from != want || inbound != inboundAddr {
		t.Fatalf("identity after send_as = %q / %q, want %q / %q", from, inbound, want, inboundAddr)
	}

	// Audit: same tx, old (null) and new value, actor recorded.
	var oldV, newV *string
	if err := tdb.Super.QueryRow(ctx, `SELECT old_value->>'send_as', new_value->>'send_as' FROM audit_entry
		WHERE action='inbound_address.send_as_updated' AND target_id=$1 AND actor_principal_id=$2 AND business_id=$3`,
		addrID, rt.owner, rt.master).Scan(&oldV, &newV); err != nil {
		t.Fatalf("read send_as audit: %v", err)
	}
	if oldV != nil || newV == nil || *newV != want {
		t.Fatalf("audit old=%v new=%v, want nil / %q", oldV, newV, want)
	}

	// A domain other than the verified profile's → 400, value unchanged.
	// Interior whitespace on the right domain would pass the domain rule but fails
	// inbound_address_send_as_chk; it must be a 400, never a CHECK violation (500).
	for _, bad := range []string{"support@evil.example.com", "support@support." + apex, "not-an-email", "a@b@" + apex,
		"sup port@" + apex, "sup\tport@" + apex, "sup\u00a0port@" + apex} {
		if _, err := svc.SetInboundAddressSendAs(ctx, rt.owner, rt.master, addrID, strp(bad)); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("send_as %q: want ErrValidation, got %v", bad, err)
		}
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM inbound_address WHERE id=$1 AND send_as=$2`, addrID, want); n != 1 {
		t.Fatalf("rejected updates changed send_as (match count %d)", n)
	}

	// Clear with null and with "" — the DEFINER falls back to the address; audit has old value.
	for _, clear := range []*string{nil, strp("  ")} {
		got, err = svc.SetInboundAddressSendAs(ctx, rt.owner, rt.master, addrID, clear)
		if err != nil || got.SendAs != nil {
			t.Fatalf("clear: send_as=%v err=%v, want nil/nil", got.SendAs, err)
		}
	}
	if from, _ := resendIdentityFrom(ctx, t, tdb, rt); from != inboundAddr {
		t.Fatalf("identity after clear = %q, want %q", from, inboundAddr)
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM audit_entry
		WHERE action='inbound_address.send_as_updated' AND target_id=$1 AND old_value->>'send_as'=$2 AND new_value->'send_as'='null'::jsonb`,
		addrID, want); n != 1 {
		t.Fatalf("clear audit rows with old=%q = %d, want 1", want, n)
	}
}

func TestIdentitySetInboundAddressSendAsRequiresVerifiedProfile(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := newIdentityService(tdb, newStubResolver())
	apex, addrID := seedSendAsFixture(ctx, t, tdb, rt, "unverified")

	_, err := svc.SetInboundAddressSendAs(ctx, rt.owner, rt.master, addrID, strp("support@"+apex))
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("unverified profile: want ErrValidation, got %v", err)
	}
	// The message reaches the settings UI verbatim, so it must stay operator-readable.
	if want := "send_as must be on the domain of the verified Resend sending profile"; err.Error() != want {
		t.Fatalf("validation message = %q, want %q", err.Error(), want)
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM inbound_address WHERE id=$1 AND send_as IS NOT NULL`, addrID); n != 0 {
		t.Fatal("send_as persisted against an unverified profile")
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM audit_entry WHERE action='inbound_address.send_as_updated' AND target_id=$1`, addrID); n != 0 {
		t.Fatalf("rejected update wrote %d audit rows, want 0", n)
	}
}

func TestIdentitySetInboundAddressSendAsOwnership(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	other := seedReadTenant(ctx, t, tdb)
	svc := newIdentityService(tdb, newStubResolver())
	apex, addrID := seedSendAsFixture(ctx, t, tdb, rt, "verified")
	otherApex, otherAddrID := seedSendAsFixture(ctx, t, tdb, other, "verified")

	// Another tenant's address, through the caller's own business or the foreign one,
	// and an unknown id all collapse to not-found.
	for name, call := range map[string]func() error{
		"foreign address via own business": func() error {
			_, err := svc.SetInboundAddressSendAs(ctx, rt.owner, rt.master, otherAddrID, strp("support@"+otherApex))
			return err
		},
		"foreign business": func() error {
			_, err := svc.SetInboundAddressSendAs(ctx, rt.owner, other.master, otherAddrID, strp("support@"+otherApex))
			return err
		},
		"unknown address": func() error {
			_, err := svc.SetInboundAddressSendAs(ctx, rt.owner, rt.master, uuid.New(), strp("support@"+apex))
			return err
		},
	} {
		if err := call(); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", name, err)
		}
	}
	if n := countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM inbound_address WHERE id=$1 AND send_as IS NOT NULL`, otherAddrID); n != 0 {
		t.Fatal("foreign address send_as was modified")
	}

	// Another business's verified profile domain does not qualify this business's address.
	if _, err := svc.SetInboundAddressSendAs(ctx, rt.owner, rt.master, addrID, strp("support@"+otherApex)); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("other business's profile domain: want ErrValidation, got %v", err)
	}

	// A system address is owned but never takes a send_as.
	sysID := uuid.New()
	if _, err := tdb.Super.Exec(ctx, `INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,created_at,updated_at)
		VALUES ($1,$2,$3,$4,'system',now(),now())`, sysID, rt.master, rt.tenantRootID, "sys-"+sysID.String()[:8]+"@mail.manyforge.example"); err != nil {
		t.Fatalf("seed system address: %v", err)
	}
	if _, err := svc.SetInboundAddressSendAs(ctx, rt.owner, rt.master, sysID, strp("support@"+apex)); !errors.Is(err, errs.ErrValidation) {
		t.Errorf("system address: want ErrValidation, got %v", err)
	}
}

// TestResendIdentityRevalidatesSendAsDomain (SECREVIEW-L2) — send_as is checked
// against the profile domain only when it is written. If the profile's from_email
// later moves to another domain, get_resend_support_identity must stop using the
// stale send_as and fall back to the inbound address (it re-checks at read time).
func TestResendIdentityRevalidatesSendAsDomain(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := newIdentityService(tdb, newStubResolver())
	apex, addrID := seedSendAsFixture(ctx, t, tdb, rt, "verified")
	inboundAddr := "support@support." + apex
	sendAs := "support@" + apex

	if _, err := svc.SetInboundAddressSendAs(ctx, rt.owner, rt.master, addrID, strp(sendAs)); err != nil {
		t.Fatalf("SetInboundAddressSendAs: %v", err)
	}
	if from, _ := resendIdentityFrom(ctx, t, tdb, rt); from != sendAs {
		t.Fatalf("identity with matching profile = %q, want %q", from, sendAs)
	}

	// Case-only difference in the profile domain still matches.
	if _, err := tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile SET from_email=$1 WHERE business_id=$2`,
		"Support@"+strings.ToUpper(apex), rt.master); err != nil {
		t.Fatal(err)
	}
	if from, _ := resendIdentityFrom(ctx, t, tdb, rt); from != sendAs {
		t.Fatalf("identity with case-different profile domain = %q, want %q", from, sendAs)
	}

	// Rotate the profile to another domain: the stale send_as is no longer used.
	if _, err := tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile SET from_email=$1 WHERE business_id=$2`,
		"support@rotated-"+apex, rt.master); err != nil {
		t.Fatal(err)
	}
	if from, inbound := resendIdentityFrom(ctx, t, tdb, rt); from != inboundAddr || inbound != inboundAddr {
		t.Fatalf("identity after profile domain change = %q / %q, want %q for both", from, inbound, inboundAddr)
	}

	// Back on the original domain but no longer verified: send_as is not used either.
	if _, err := tdb.Super.Exec(ctx, `UPDATE mailing_sending_profile SET from_email=$1, status='unverified' WHERE business_id=$2`,
		sendAs, rt.master); err != nil {
		t.Fatal(err)
	}
	if from, _ := resendIdentityFrom(ctx, t, tdb, rt); from != inboundAddr {
		t.Fatalf("identity with unverified profile = %q, want %q", from, inboundAddr)
	}
}
