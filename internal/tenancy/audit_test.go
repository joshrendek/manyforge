//go:build integration

package tenancy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/tenancy"
)

func TestAuditRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}
	adminRole := presetRole(ctx, t, tdb, "admin")
	memberRoleID := presetRole(ctx, t, tdb, "member")
	viewerRole := presetRole(ctx, t, tdb, "viewer")

	owner, master := seedFounder(ctx, t, tdb, "au-owner@x.test")
	bob := seedMemberAt(ctx, t, tdb, master, master, memberRoleID, "au-bob@x.test")
	viewer := seedMemberAt(ctx, t, tdb, master, master, viewerRole, "au-viewer@x.test")

	// Produce two audited mutations on the master.
	if err := svc.ChangeMemberRole(ctx, owner, master, bob, adminRole); err != nil {
		t.Fatalf("change role: %v", err)
	}
	if err := svc.RevokeMember(ctx, owner, master, bob); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	t.Run("audit.read holder reads the trail, newest first", func(t *testing.T) {
		entries, _, err := svc.ListAudit(ctx, owner, master, "", 50)
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		if len(entries) < 2 {
			t.Fatalf("want >=2 audit entries, got %d", len(entries))
		}
		// Newest first: the revoke is the most recent mutation.
		if entries[0].Action != "membership.revoked" {
			t.Errorf("newest entry: want membership.revoked, got %q", entries[0].Action)
		}
	})

	t.Run("gate: viewer without audit.read -> 404", func(t *testing.T) {
		if _, _, err := svc.ListAudit(ctx, viewer, master, "", 50); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("viewer audit read: want ErrNotFound, got %v", err)
		}
	})

	t.Run("pagination: cursor walks the trail without overlap", func(t *testing.T) {
		first, cursor, err := svc.ListAudit(ctx, owner, master, "", 1)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if len(first) != 1 || cursor == nil {
			t.Fatalf("page 1: want 1 entry + next cursor, got %d entries cursor=%v", len(first), cursor)
		}
		second, _, err := svc.ListAudit(ctx, owner, master, *cursor, 1)
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if len(second) != 1 {
			t.Fatalf("page 2: want 1 entry, got %d", len(second))
		}
		if second[0].ID == first[0].ID {
			t.Errorf("pagination overlap: page 2 repeated page 1's entry %s", first[0].ID)
		}
	})
}
