//go:build integration

package notify

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

// ---------------------------------------------------------------------------
// T054 — [US4] DKIM-signing RED-GATE (verified-domain reply signed; system fallback).
//
// This file pins the AGREED interface contract
// (/tmp/mf-us4-interface-contract.md, sections "Outbound identity selection
// (T059)" and "DKIM signing test (T054)") for the OUTBOUND send path: a reply
// from a business with a verified custom email_domain must go out FROM the
// custom support@<domain> address AND be DKIM-signed with that domain's
// per-domain selector + Ed25519 key, while a business with NO verified domain
// falls back to the system identity unsigned-by-custom-identity (FR-013).
//
// EXPECTED RED — production for T059 does NOT exist yet. Specifically:
//   - notify.SendSubscriber has NO `Sealer KeySealer` field yet (the contract
//     adds it in T059). Referencing it here is a COMPILE error → expected red.
//   - notify.Mail has NO `DKIM *DKIMConfig` field yet — the seam by which
//     Handle hands the per-domain signing config to the Sender (see SEAM note
//     below). Referencing it is a COMPILE error → expected red.
//   - The `get_send_identity(business_id, tenant_root_id)` SECURITY DEFINER
//     (new migration 0023, owned by T059) does NOT exist, so Handle cannot
//     select the custom identity; even once the fields compile, the custom
//     From + DKIM config would be absent → runtime red.
// T059 implements all three and turns this green. Because Go can't compile the
// package while SendSubscriber.Sealer / Mail.DKIM are undefined, the other
// notify integration tests (send/suppression) also won't run during the red
// window — acceptable for this TDD gate (same convention as T053's
// identity_integration_test.go and the triage red-gate).
//
// SEAM (where signing must happen — high-value note for T059):
// Today DKIM signing lives INSIDE SMTPSender.Send: it calls buildMIME(m) then,
// if s.cfg.DKIM != nil, signDKIM(raw, *s.cfg.DKIM) (smtp.go:57-67). That single
// process-wide cfg.DKIM cannot express a PER-DOMAIN identity chosen per message.
// The contract puts identity SELECTION in SendSubscriber.Handle (call
// get_send_identity, Sealer.Open the key, build a DKIMConfig). The minimal,
// observable seam consistent with "sign via existing signDKIM" is therefore:
// Handle attaches the chosen per-domain DKIMConfig to the outgoing Mail
// (new field Mail.DKIM *DKIMConfig) and the Sender signs with it (the existing
// SMTPSender.Send signs `m.DKIM` when set, instead of only the static
// s.cfg.DKIM). That keeps signing in one place (buildMIME+signDKIM) while
// letting Handle pick the identity. This test OBSERVES the signed wire bytes by
// using a capturing Sender that mirrors SMTPSender.Send's build+sign exactly
// (buildMIME then signDKIM when Mail.DKIM is set) — so the captured raw is
// byte-for-byte what the real SMTPSender would transmit — and verifies it with
// go-msgauth.
// ---------------------------------------------------------------------------

// dkimSealer is a TEST-ONLY KeySealer stub: Seal base64-encodes the plaintext
// and Open decodes it. NOT encryption — it exists only so this red-gate runs
// without depending on T057's AES-256-GCM sealer. Open(Seal(x)) == x is the only
// KeySealer property this test relies on (copied from T053's b64Sealer; kept
// local because the notify package owns its own test fixtures). The injected
// instance is referenced through SendSubscriber.Sealer, which T059 adds.
type dkimSealer struct{}

func (dkimSealer) Seal(plaintext []byte) (string, error) {
	return base64.StdEncoding.EncodeToString(plaintext), nil
}

func (dkimSealer) Open(ref string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(ref)
}

// wireCaptureSender records the raw RFC822 wire bytes a real SMTPSender would
// transmit. It mirrors SMTPSender.Send's build+sign (smtp.go:57-67) EXACTLY —
// buildMIME(m), then signDKIM(raw, *m.DKIM) when the per-domain DKIM config is
// attached — so the captured bytes are the same bytes that would reach the MTA.
// (We capture here, in the Sender, rather than refactoring SMTPSender to take an
// injectable net/smtp transport: the contract's seam is "Handle selects the
// identity + DKIMConfig", and the Sender remains the build+sign chokepoint.)
type wireCaptureSender struct {
	lastMail *Mail
	lastRaw  []byte
	calls    int
}

func (c *wireCaptureSender) Send(_ context.Context, m Mail) error {
	c.calls++
	mc := m
	c.lastMail = &mc
	raw, err := buildMIME(m)
	if err != nil {
		return err
	}
	// Mirror SMTPSender.Send: sign with the per-message DKIM config when present.
	// Mail.DKIM is the T059 seam (undefined today → compile red).
	if m.DKIM != nil {
		signed, serr := signDKIM(raw, *m.DKIM)
		if serr != nil {
			return serr
		}
		raw = signed
	}
	c.lastRaw = raw
	return nil
}

// seedVerifiedDKIMDomain plants, via the RLS-exempt Super pool, a VERIFIED
// email_domain with a real Ed25519 keypair + selector + DNS-publishable public
// key, plus a CUSTOM inbound_address (support@<domain>) on it — exactly the
// shape get_send_identity (T059) selects. The private key is sealed with the
// in-test dkimSealer and stored in dkim_private_key_ref. Returns the domain, the
// selector, the published DKIM TXT value, and the custom from-address.
func seedVerifiedDKIMDomain(ctx context.Context, t *testing.T, tdb *testdb.TestDB, st sendTenant) (domain, selector, dkimTXT, customAddr string) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	sealed, err := dkimSealer{}.Seal(priv)
	if err != nil {
		t.Fatalf("seal private key: %v", err)
	}

	domain = "brand-" + st.master.String()[:8] + ".example.com"
	selector = "mf" + st.master.String()[:8]
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	dkimTXT = "v=DKIM1; k=ed25519; p=" + pubB64
	customAddr = "support@" + domain

	domainID := uuid.New()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO email_domain
		   (id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,
		    dkim_selector,dkim_public_key,dkim_private_key_ref,spf_state,created_at,updated_at)
		 VALUES ($1,$2,$2,$3,'provider_route','mf-verify=seedtoken',now(),
		    $4,$5,$6,'pass',now(),now())`,
		domainID, st.master, domain, selector, dkimTXT, sealed); err != nil {
		t.Fatalf("seed verified email_domain: %v", err)
	}
	// Custom inbound address on the verified domain — the from_address
	// get_send_identity returns.
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO inbound_address
		   (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at)
		 VALUES ($1,$2,$2,$3,'custom',$4,now(),now())`,
		uuid.New(), st.master, customAddr, domainID); err != nil {
		t.Fatalf("seed custom inbound_address: %v", err)
	}
	return domain, selector, dkimTXT, customAddr
}

// TestSendDKIMSignsReplyFromVerifiedDomain — a reply for a business that owns a
// VERIFIED custom email_domain (with a per-domain selector/Ed25519 key) must be
// sent FROM the custom support@<domain> address AND carry a valid DKIM-Signature
// for that domain+selector. Verified with go-msgauth against the published
// DKIM TXT (offline stub lookup).
func TestSendDKIMSignsReplyFromVerifiedDomain(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	domain, selector, dkimTXT, customAddr := seedVerifiedDKIMDomain(ctx, t, tdb, st)

	cap := &wireCaptureSender{}
	// SendSubscriber.Sealer is the T059 field (undefined today → compile red).
	sub := SendSubscriber{Sender: cap, Sealer: dkimSealer{}}

	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, repliedEvent(t, st))
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if cap.lastMail == nil {
		t.Fatalf("sender not called")
	}

	// (1) From (and thus DKIM d=) is the CUSTOM domain address, NOT the system one.
	if cap.lastMail.From != customAddr {
		t.Errorf("Mail.From = %q, want custom %q (verified-domain identity, not system %q)",
			cap.lastMail.From, customAddr, st.sysAddr)
	}

	raw := cap.lastRaw
	rawStr := string(raw)

	// (2) A DKIM-Signature header is present with the per-domain selector + domain.
	if !strings.Contains(rawStr, "DKIM-Signature:") {
		t.Fatalf("no DKIM-Signature header on a reply from a verified domain\n---\n%s", rawStr)
	}
	if !strings.Contains(rawStr, "s="+selector) {
		t.Errorf("DKIM-Signature missing s=%s\n---\n%s", selector, rawStr)
	}
	if !strings.Contains(rawStr, "d="+domain) {
		t.Errorf("DKIM-Signature missing d=%s\n---\n%s", domain, rawStr)
	}

	// (3) Cryptographically verify the signature with go-msgauth against the
	// published Ed25519 public key (offline stub TXT lookup). go-msgauth calls
	// LookupTXT with "<selector>._domainkey.<domain>".
	wantQuery := selector + "._domainkey." + domain
	stubLookup := func(name string) ([]string, error) {
		if name == wantQuery {
			return []string{dkimTXT}, nil
		}
		return nil, nil
	}
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{LookupTXT: stubLookup})
	if err != nil {
		t.Fatalf("dkim.VerifyWithOptions: %v", err)
	}
	if len(verifs) != 1 {
		t.Fatalf("got %d DKIM verifications, want exactly 1", len(verifs))
	}
	v := verifs[0]
	if v.Err != nil {
		t.Errorf("DKIM verification failed: %v (want valid Ed25519 signature)", v.Err)
	}
	if v.Domain != domain {
		t.Errorf("DKIM verified domain = %q, want %q", v.Domain, domain)
	}
}

// TestSendSystemFallbackNotCustomSigned — FR-013: the SAME send path for a
// business with NO verified custom domain must send FROM the SYSTEM address and
// must NOT be signed as any custom domain. (It may carry the system DKIM if one
// is configured; here none is, so the message is unsigned — the load-bearing
// assertion is the ABSENCE of a custom-domain d= signature, never the system
// identity masquerading as the brand.)
func TestSendSystemFallbackNotCustomSigned(t *testing.T) {
	ctx, tdb := startSendDB(t)
	// No verified domain seeded for this tenant.
	st := seedSendTenant(ctx, t, tdb, "pending")

	cap := &wireCaptureSender{}
	sub := SendSubscriber{Sender: cap, Sealer: dkimSealer{}}

	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return sub.Handle(ctx, tx, repliedEvent(t, st))
	}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if cap.lastMail == nil {
		t.Fatalf("sender not called")
	}

	// From is the SYSTEM address (no custom identity to fall forward to).
	if cap.lastMail.From != st.sysAddr {
		t.Errorf("Mail.From = %q, want system %q (FR-013 fallback)", cap.lastMail.From, st.sysAddr)
	}
	// No per-message custom DKIM config selected.
	if cap.lastMail.DKIM != nil {
		t.Errorf("Mail.DKIM = %+v, want nil for a business with no verified domain", cap.lastMail.DKIM)
	}
	// The wire bytes must NOT be signed as a custom domain. Any DKIM-Signature
	// present must not name a custom-domain d= (there is none here at all).
	if bytes.Contains(cap.lastRaw, []byte("DKIM-Signature:")) {
		t.Errorf("system-fallback message is DKIM-signed, want unsigned-by-custom-identity\n---\n%s", cap.lastRaw)
	}
}
