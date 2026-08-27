//go:build integration

package mailing_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/mailing"
	mfcrypto "github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/secrets"
)

type mailingSeed struct{ businessID, principalID uuid.UUID }

func seedMailingTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) mailingSeed {
	t.Helper()
	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatal(err)
	}
	s := mailingSeed{uuid.New(), uuid.New()}
	accountID := uuid.New()
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	statements := []struct {
		q    string
		args []any
	}{{"INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())", []any{accountID, "mailing-" + s.businessID.String() + "@x.test"}}, {"INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())", []any{s.principalID, accountID}}, {"INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'MailCo','active',now(),now())", []any{s.businessID}}, {"INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)", []any{s.businessID}}, {"INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())", []any{s.principalID, s.businessID, ownerRole}}}
	for _, st := range statements {
		if _, err = tx.Exec(ctx, st.q, st.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func seedSibling(ctx context.Context, t *testing.T, tdb *testdb.TestDB, root uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,$2,$2,'Sibling','active',now(),now())", id, root); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$2),($2,$1,1,$2)", id, root); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMailingLifecycleAndIsolation(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer tdb.Close(ctx)
	a := seedMailingTenant(ctx, t, tdb)
	b := seedMailingTenant(ctx, t, tdb)
	sibling := seedSibling(ctx, t, tdb, a.businessID)
	svc := &mailing.Service{DB: tdb.App}
	listA, err := svc.CreateList(ctx, a.principalID, a.businessID, mailing.ListInput{Name: "News", DoubleOptIn: false})
	if err != nil {
		t.Fatalf("CreateList: %v", err)
	}
	firstName := "Alice"
	sub, err := svc.CreateSubscriber(ctx, a.principalID, a.businessID, listA.ID, mailing.SubscriberInput{Email: "Alice@Example.com", FirstName: &firstName, Tags: []string{"VIP"}, ConsentSource: "manual"})
	if err != nil {
		t.Fatalf("CreateSubscriber: %v", err)
	}
	if sub.Status != "active" || sub.Email != "alice@example.com" || len(sub.Tags) != 1 {
		t.Fatalf("subscriber = %+v", sub)
	}
	cleared, err := svc.UpdateSubscriber(ctx, a.principalID, a.businessID, listA.ID, sub.ID, mailing.SubscriberUpdate{SetFirstName: true, Tags: &[]string{}})
	if err != nil {
		t.Fatalf("UpdateSubscriber clear: %v", err)
	}
	if cleared.FirstName != nil || len(cleared.Tags) != 0 {
		t.Fatalf("cleared subscriber = %+v", cleared)
	}
	if _, err = svc.GetList(ctx, b.principalID, b.businessID, listA.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("foreign tenant list error = %v", err)
	}
	if _, err = svc.GetList(ctx, a.principalID, sibling, listA.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("same-root sibling list error = %v", err)
	}
	listSibling, err := svc.CreateList(ctx, a.principalID, sibling, mailing.ListInput{Name: "Sibling News", DoubleOptIn: true})
	if err != nil {
		t.Fatalf("CreateList sibling: %v", err)
	}
	if _, err = svc.GetSubscriber(ctx, a.principalID, sibling, listSibling.ID, sub.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-list subscriber error = %v", err)
	}
	if err = svc.ArchiveList(ctx, a.principalID, a.businessID, listA.ID); err != nil {
		t.Fatalf("ArchiveList: %v", err)
	}
	if _, err = svc.CreateSubscriber(ctx, a.principalID, a.businessID, listA.ID, mailing.SubscriberInput{Email: "late@example.com", ConsentSource: "manual"}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("subscriber on archived list error = %v", err)
	}
	if _, err = svc.CreateListKey(ctx, a.principalID, a.businessID, listA.ID, nil); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("key on archived list error = %v", err)
	}
	sealer, err := mfcrypto.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc.Sealer = sealer
	svc.Vault = secrets.NewVault(sealer)
	createdKey, err := svc.CreateListKey(ctx, a.principalID, sibling, listSibling.ID, nil)
	if err != nil {
		t.Fatalf("CreateListKey: %v", err)
	}
	if !strings.HasPrefix(createdKey.Secret, "mls_") {
		t.Fatalf("create secret = %q", createdKey.Secret)
	}
	keys, err := svc.ListListKeys(ctx, a.principalID, sibling, listSibling.ID, 10)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListListKeys = %+v, err=%v", keys, err)
	}
	if keys[0].Secret != "" || !keys[0].HasSecret {
		t.Fatalf("listed key leaked/missed secret metadata: %+v", keys[0])
	}
	profile, err := svc.PutSendingProfile(ctx, a.principalID, a.businessID, mailing.SendingProfileInput{Mode: "resend", FromEmail: "sender@example.com", FromName: "Sender", Resend: &mailing.ResendCredentials{APIKey: "re_secret"}})
	if err != nil {
		t.Fatalf("PutSendingProfile: %v", err)
	}
	raw, _ := json.Marshal(profile)
	if strings.Contains(string(raw), "re_secret") || strings.Contains(string(raw), "secret_ref") {
		t.Fatalf("profile response leaks credential material: %s", raw)
	}
	if !profile.HasCredentials {
		t.Fatal("profile should report credentials")
	}
	if _, err = svc.PutSendingProfile(ctx, a.principalID, a.businessID, mailing.SendingProfileInput{Mode: "resend", FromEmail: "sender@example.com", FromName: "Sender", Resend: &mailing.ResendCredentials{APIKey: "re_rotated"}}); err != nil {
		t.Fatalf("rotate profile: %v", err)
	}
	var secretCount int
	if err = tdb.Super.QueryRow(ctx, "SELECT count(*) FROM secret WHERE business_id=$1 AND scope='mailing'", a.businessID).Scan(&secretCount); err != nil || secretCount != 1 {
		t.Fatalf("mailing secret count = %d, err=%v", secretCount, err)
	}
	if err = svc.DeleteSendingProfile(ctx, a.principalID, a.businessID); err != nil {
		t.Fatalf("DeleteSendingProfile: %v", err)
	}
	if err = tdb.Super.QueryRow(ctx, "SELECT count(*) FROM secret WHERE business_id=$1 AND scope='mailing'", a.businessID).Scan(&secretCount); err != nil || secretCount != 0 {
		t.Fatalf("mailing secret count after delete = %d, err=%v", secretCount, err)
	}
	if err = tdb.App.WithPrincipal(ctx, b.principalID, func(tx pgx.Tx) error {
		_, err := dbgen.New(tx).GetMailingList(ctx, dbgen.GetMailingListParams{ID: listA.ID, TenantRootID: a.businessID})
		return err
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("RLS direct read error = %v", err)
	}
}
