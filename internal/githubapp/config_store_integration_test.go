//go:build integration

package githubapp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/manyforge/manyforge/internal/githubapp"
	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestConfigStoreSaveIsNonOverwritable(t *testing.T) {
	ctx := context.Background()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("testdb.Start: %v", err)
	}
	defer tdb.Close(ctx)
	sealer, _ := crypto.NewSealer(make([]byte, 32))
	store := &githubapp.ConfigStore{DB: tdb.App, Sealer: sealer}

	creds := githubapp.AppCreds{AppID: 42, Slug: "mf-review", ClientID: "Iv1.fake",
		ClientSecret: "cs_fake", PrivateKeyPEM: "-----BEGIN RSA PRIVATE KEY-----fake", WebhookSecret: "whsec_fake"}
	if err := store.Save(ctx, creds); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := store.Save(ctx, creds); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("second Save = %v, want ErrConflict", err)
	}

	got, err := store.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AppID != 42 || got.WebhookSecret != "whsec_fake" || got.PrivateKeyPEM != creds.PrivateKeyPEM {
		t.Fatalf("Get returned %+v", got)
	}
}
