// Command seeddemo idempotently fills a dev database with a demo support desk:
// the live-demo user, the Acme Holdings business tree, each business's system
// inbound address, and a handful of threaded conversations ingested through the
// REAL inbox pipeline. Re-running is a no-op. App-role only (no superuser needed).
//
//	make seed-demo   # loads .air.env then runs this
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/tenancy"
)

const (
	demoEmail    = "live-demo@manyforge.test"
	demoName     = "Live Demo"
	demoPassword = "DevPassw0rd!"
	masterName   = "Acme Holdings"
)

var subNames = []string{"Engineering", "Platform Team", "Sales"}

// bizSlug maps a business name to the short slug used in fixture message-ids.
func bizSlug(name string) string {
	switch name {
	case masterName:
		return "acme"
	case "Engineering":
		return "eng"
	case "Platform Team":
		return "plat"
	case "Sales":
		return "sales"
	default:
		return "biz"
	}
}

func main() {
	if err := run(); err != nil {
		slog.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.DatabaseURL == "" {
		return errors.New("MANYFORGE_DATABASE_URL is required (source .air.env)")
	}
	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	principalID, err := ensureUser(ctx, database, logger)
	if err != nil {
		return err
	}
	logger.Info("demo user ready", "principal", principalID)

	businesses, err := ensureBusinesses(ctx, database, principalID, logger)
	if err != nil {
		return err
	}

	// System inbound addresses, provisioned synchronously via the real Provisioner.
	prov := inbox.NewProvisioner(database, inbox.ProvisionConfig{
		SystemDomain: cfg.InboundSystemDomain,
		SystemKey:    cfg.InboundSystemAddrSecret,
	}, logger)
	for _, b := range businesses {
		if err := provisionAddress(ctx, database, prov, b); err != nil {
			return fmt.Errorf("provision %s: %w", b.Name, err)
		}
	}

	// Real ingestion. Attachments unused, so a throwaway file:// store satisfies the ctor.
	store, err := blob.Open(ctx, "file://"+os.TempDir()+"/manyforge-seed-blobs")
	if err != nil {
		return fmt.Errorf("open blob store: %w", err)
	}
	defer func() { _ = store.Close() }()
	inboxSvc := inbox.NewService(database, store, inbox.Config{
		ReplyTokenKey:       cfg.InboundReplyTokenSecret,
		AttachmentMaxBytes:  cfg.AttachmentMaxBytes,
		InboundSystemDomain: cfg.InboundSystemDomain,
	}, logger)

	created, dup := 0, 0
	for _, b := range businesses {
		addr := systemAddress(cfg.InboundSystemAddrSecret, cfg.InboundSystemDomain, b.ID)
		for _, conv := range conversationsFor(bizSlug(b.Name)) {
			for _, m := range conv.Msgs {
				res, err := inboxSvc.Ingest(ctx, inbox.RawMessage{
					Provider:          "seed",
					EnvelopeRecipient: addr,
					EnvelopeSender:    m.From,
					ReceivedAt:        time.Now(),
					Raw:               rfc822(m.From, addr, m.Subject, m.MessageID, m.InReplyTo, m.Body),
				})
				if err != nil {
					return fmt.Errorf("ingest %s: %w", m.MessageID, err)
				}
				if res.Duplicate {
					dup++
				} else {
					created++
				}
			}
		}
		logger.Info("seeded business", "name", b.Name, "address", addr)
	}
	logger.Info("seed complete", "messages_created", created, "messages_skipped_duplicate", dup)
	return nil
}

// ensureUser returns the demo human principal id, creating + verifying the account
// on first run. account/principal are not RLS-protected, so a plain WithTx works.
func ensureUser(ctx context.Context, database *db.DB, logger *slog.Logger) (uuid.UUID, error) {
	acctSvc := &account.Service{DB: database, TokenTTL: 24 * time.Hour, Now: time.Now}

	var principalID uuid.UUID
	lookup := func() (bool, error) {
		var found bool
		err := database.WithTx(ctx, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			acc, err := q.GetAccountByEmail(ctx, demoEmail)
			if err != nil {
				return nil // treat as not-found; caller will create
			}
			prin, err := q.GetPrincipalByAccount(ctx, db.PGUUID(acc.ID))
			if err != nil {
				return err
			}
			principalID = prin.ID
			found = true
			return nil
		})
		return found, err
	}

	found, err := lookup()
	if err != nil {
		return uuid.Nil, fmt.Errorf("lookup user: %w", err)
	}
	if found {
		return principalID, nil
	}

	_, token, err := acctSvc.Signup(ctx, demoEmail, demoName, demoPassword)
	if err != nil {
		return uuid.Nil, fmt.Errorf("signup: %w", err)
	}
	if err := acctSvc.VerifyEmail(ctx, token); err != nil {
		return uuid.Nil, fmt.Errorf("verify: %w", err)
	}
	logger.Info("created demo account", "email", demoEmail)

	found, err = lookup()
	if err != nil || !found {
		return uuid.Nil, fmt.Errorf("principal not found after signup: %w", err)
	}
	return principalID, nil
}

// ensureBusinesses returns the master + three subs, creating any that are missing.
func ensureBusinesses(ctx context.Context, database *db.DB, principalID uuid.UUID, logger *slog.Logger) ([]tenancy.Business, error) {
	ten := &tenancy.Service{DB: database}
	existing, err := ten.ListBusinesses(ctx, principalID)
	if err != nil {
		return nil, fmt.Errorf("list businesses: %w", err)
	}
	byName := map[string]tenancy.Business{}
	for _, b := range existing {
		byName[b.Name] = b
	}

	master, ok := byName[masterName]
	if !ok {
		master, err = ten.CreateMasterBusiness(ctx, principalID, masterName)
		if err != nil {
			return nil, fmt.Errorf("create master: %w", err)
		}
		logger.Info("created master business", "name", masterName)
	}
	out := []tenancy.Business{master}
	for _, name := range subNames {
		if b, ok := byName[name]; ok {
			out = append(out, b)
			continue
		}
		b, err := ten.CreateSubBusiness(ctx, principalID, master.ID, name)
		if err != nil {
			return nil, fmt.Errorf("create sub %s: %w", name, err)
		}
		logger.Info("created sub business", "name", name)
		out = append(out, b)
	}
	return out, nil
}

// provisionAddress drives the REAL Provisioner.Handle in a principal-less tx,
// inserting the deterministic system address (idempotent: a replay is a no-op).
func provisionAddress(ctx context.Context, database *db.DB, prov *inbox.Provisioner, b tenancy.Business) error {
	payload, err := json.Marshal(map[string]any{
		"business_id":    b.ID,
		"tenant_root_id": b.TenantRootID,
	})
	if err != nil {
		return err
	}
	return database.WithTx(ctx, func(tx pgx.Tx) error {
		return prov.Handle(ctx, tx, events.Event{Topic: events.TopicBusinessCreated, Payload: payload})
	})
}

// rfc822 builds a minimal text/plain message (mirrors internal/inbox test helper).
func rfc822(from, to, subject, messageID, inReplyTo, body string) []byte {
	msg := "From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Mon, 02 Jan 2006 15:04:05 -0700\r\n" +
		"Message-ID: <" + messageID + ">\r\n"
	if inReplyTo != "" {
		msg += "In-Reply-To: <" + inReplyTo + ">\r\n" +
			"References: <" + inReplyTo + ">\r\n"
	}
	msg += "MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
	return []byte(msg)
}

// systemAddress re-derives a business's deterministic system inbound address. This
// MUST match internal/inbox/provision.go's derivation exactly (same key + length).
func systemAddress(key []byte, domain string, businessID uuid.UUID) string {
	mac := hmac.New(sha256.New, key)
	id := businessID
	mac.Write(id[:])
	return fmt.Sprintf("b-%s@%s", hex.EncodeToString(mac.Sum(nil))[:16], domain)
}
