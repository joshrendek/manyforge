//go:build integration

package mailing_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

func supportProfileID(ctx context.Context, t *testing.T, tdb *testdb.TestDB, businessID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM mailing_sending_profile WHERE business_id=$1", businessID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSendSupportReplyUsesProfileAndRefusesWhenUnverified(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, captured := campaignService(t, ctx, tdb, seed)
	profileID := supportProfileID(ctx, t, tdb, seed.businessID)

	mail := notify.Mail{From: "support@support.example.test", To: "ada@customer.test", Subject: "Re: Help",
		BodyText: "hi", MessageID: "m-1@inbound.localhost", ReplyTo: "support+Tok@support.example.test"}
	if err := svc.SendSupportReply(ctx, profileID, mail); err != nil {
		t.Fatalf("SendSupportReply: %v", err)
	}
	if captured.mail.From != mail.From || captured.mail.ReplyTo != mail.ReplyTo || captured.mail.To != mail.To {
		t.Fatalf("delivered mail = %+v", captured.mail)
	}

	if _, err := tdb.Super.Exec(ctx, "UPDATE mailing_sending_profile SET status='error' WHERE id=$1", profileID); err != nil {
		t.Fatal(err)
	}
	before := len(captured.mails)
	if err := svc.SendSupportReply(ctx, profileID, mail); !errors.Is(err, mailing.ErrSupportProfileUnavailable) {
		t.Fatalf("unverified profile error = %v, want ErrSupportProfileUnavailable", err)
	}
	if len(captured.mails) != before {
		t.Fatal("an unverified profile still sent")
	}
}

// Review Focus 5 (outbound): a rotated API key is picked up on the next send
// because the provider cache is keyed by the profile's updated_at.
func TestSendSupportReplyUsesRotatedCredential(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close(ctx)
	seed := seedMailingTenant(ctx, t, tdb)
	svc, captured := campaignService(t, ctx, tdb, seed)
	var mu sync.Mutex
	var keys []string
	svc.Providers = mailprovider.NewCache(func(_ context.Context, p mailprovider.Profile) (mailprovider.Deliverer, error) {
		mu.Lock()
		keys = append(keys, p.ResendAPIKey)
		mu.Unlock()
		return captured, nil
	}, time.Minute)
	profileID := supportProfileID(ctx, t, tdb, seed.businessID)
	mail := notify.Mail{From: "support@support.example.test", To: "ada@customer.test", Subject: "Re", BodyText: "x"}
	if err := svc.SendSupportReply(ctx, profileID, mail); err != nil {
		t.Fatal(err)
	}
	sealed, err := svc.Sealer.Seal([]byte(`{"api_key":"re_rotated"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, `UPDATE secret SET sealed_value=$1
		WHERE id=(SELECT secret_ref FROM mailing_sending_profile WHERE id=$2)`, sealed, profileID); err != nil {
		t.Fatal(err)
	}
	if _, err := tdb.Super.Exec(ctx, "UPDATE mailing_sending_profile SET updated_at=now() + interval '1 second' WHERE id=$1", profileID); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendSupportReply(ctx, profileID, mail); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(keys) != "[re_test re_rotated]" {
		t.Fatalf("built with keys %v, want [re_test re_rotated]", keys)
	}
}
