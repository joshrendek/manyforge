//go:build integration

package provider

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/google/uuid"

	mfcrypto "github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

type relayCaptureSender struct {
	mail  notify.Mail
	calls int
}

func (s *relayCaptureSender) Send(_ context.Context, mail notify.Mail) error {
	s.calls++
	s.mail = mail
	return nil
}

func TestRelayResolvesVerifiedDKIMIdentityAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)

	businessID, domainID := uuid.New(), uuid.New()
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		 VALUES ($1,NULL,$1,'RelayCo','active',now(),now())`, businessID); err != nil {
		t.Fatalf("seed business: %v", err)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := mfcrypto.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx,
		`INSERT INTO email_domain
		   (id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,
		    dkim_selector,dkim_public_key,dkim_private_key_ref,spf_state,created_at,updated_at)
		 VALUES ($1,$2,$2,'mail.example.com','provider_route','mf-verify=test',now(),
		    'mfrelay','unused',$3,'pass',now(),now())`, domainID, businessID, sealed); err != nil {
		t.Fatalf("seed email domain: %v", err)
	}

	capture := &relayCaptureSender{}
	relay := &Relay{DB: tdb.App, Sealer: sealer, Sender: capture, EmailDomainID: domainID}
	if err := relay.Verify(ctx); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	result, err := relay.Send(ctx, notify.Mail{To: "reader@example.net", MessageID: "message@example.com"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if capture.calls != 1 || capture.mail.DKIM == nil {
		t.Fatalf("sender calls = %d, DKIM = %#v", capture.calls, capture.mail.DKIM)
	}
	if capture.mail.DKIM.Domain != "mail.example.com" || capture.mail.DKIM.Selector != "mfrelay" ||
		!privateKey.Equal(capture.mail.DKIM.PrivateKey) || result.ProviderID != "message@example.com" {
		t.Fatalf("relay identity = %#v, result = %+v", capture.mail.DKIM, result)
	}

	if _, err := tdb.Super.Exec(ctx, `UPDATE email_domain SET verified_at=NULL WHERE id=$1`, domainID); err != nil {
		t.Fatal(err)
	}
	if err := relay.Verify(ctx); err == nil {
		t.Fatal("Verify succeeded for unverified relay domain")
	}
}
