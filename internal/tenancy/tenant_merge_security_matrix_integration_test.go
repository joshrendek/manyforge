//go:build integration

package tenancy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/tenancy"
)

func TestTenantMergeAuthorizationSecurityMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	svc := &tenancy.Service{DB: tdb.App}

	sourceOwner, sourceRoot := seedFounder(
		ctx, t, tdb, "security-source-owner@x.test",
	)
	destinationOwner, destinationRoot := seedFounder(
		ctx, t, tdb, "security-destination-owner@x.test",
	)
	sourceChild, err := svc.CreateSubBusiness(
		ctx, sourceOwner, sourceRoot, "Security source child",
	)
	if err != nil {
		t.Fatalf("create source child: %v", err)
	}
	destinationParent, err := svc.CreateSubBusiness(
		ctx, destinationOwner, destinationRoot,
		"Security destination parent",
	)
	if err != nil {
		t.Fatalf("create destination parent: %v", err)
	}

	assertHidden := func(
		t *testing.T,
		actor, source, destination uuid.UUID,
		key string,
	) {
		t.Helper()
		_, err := svc.CreateTenantMergeOperation(
			ctx, actor, source, destination, key,
		)
		if !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("unauthorized/hidden merge creation = %v, want ErrNotFound",
				err)
		}
	}

	t.Run("owner in only source is ineligible", func(t *testing.T) {
		assertHidden(
			t, sourceOwner, sourceRoot, destinationParent.ID,
			"security-source-only",
		)
	})

	ownerRole := presetRole(ctx, t, tdb, "owner")
	inheritedOwner := seedMemberAt(
		ctx, t, tdb, sourceChild.ID, sourceRoot, ownerRole,
		"security-inherited-owner@x.test",
	)
	addDirectOwner(ctx, t, tdb, inheritedOwner, destinationRoot)
	t.Run("inherited owner is not a direct source owner", func(t *testing.T) {
		assertHidden(
			t, inheritedOwner, sourceRoot, destinationParent.ID,
			"security-inherited-owner",
		)
	})

	equivalentRole := customRole(
		ctx, t, tdb, sourceRoot, "Owner equivalent",
		"business.read",
		"hierarchy.manage",
		"members.manage",
		"roles.manage",
		"business.delete",
		"ownership.transfer",
		"agents.approve",
	)
	customOwner := seedMemberAt(
		ctx, t, tdb, sourceRoot, sourceRoot, equivalentRole,
		"security-custom-owner@x.test",
	)
	addDirectOwner(ctx, t, tdb, customOwner, destinationRoot)
	t.Run("custom owner-equivalent role is ineligible", func(t *testing.T) {
		assertHidden(
			t, customOwner, sourceRoot, destinationParent.ID,
			"security-custom-owner",
		)
	})

	agentPrincipal := uuid.New()
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO principal (
		    id, kind, home_business_id, tenant_root_id
		) VALUES ($1, 'agent', $2, $3)`,
		agentPrincipal, sourceChild.ID, sourceRoot,
	); err != nil {
		t.Fatalf("seed contained agent principal: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx, `
		INSERT INTO membership (
		    principal_id, business_id, tenant_root_id, role_id
		) VALUES ($1, $2, $3, $4)`,
		agentPrincipal, sourceChild.ID, sourceRoot,
		presetRole(ctx, t, tdb, "agent_runtime"),
	); err != nil {
		t.Fatalf("seed contained agent principal: %v", err)
	}
	t.Run("agent principal cannot authorize a merge", func(t *testing.T) {
		assertHidden(
			t, agentPrincipal, sourceRoot, destinationParent.ID,
			"security-agent",
		)
	})

	t.Run("unknown and hidden IDs remain indistinguishable", func(t *testing.T) {
		assertHidden(
			t, sourceOwner, uuid.New(), destinationParent.ID,
			"security-unknown-source",
		)
		assertHidden(
			t, sourceOwner, sourceRoot, uuid.New(),
			"security-unknown-destination",
		)
	})

	addDirectOwner(ctx, t, tdb, sourceOwner, destinationRoot)
	t.Run("one human directly owning both roots is eligible", func(t *testing.T) {
		operation, err := svc.CreateTenantMergeOperation(
			ctx, sourceOwner, sourceRoot, destinationParent.ID,
			"security-direct-owner-both",
		)
		if err != nil {
			t.Fatalf("direct owner of both roots: %v", err)
		}
		if operation.ActorPrincipalID != sourceOwner ||
			operation.SourceRootID != sourceRoot ||
			operation.DestinationRootID != destinationRoot {
			t.Errorf("authorized operation binding = %+v", operation)
		}
	})
}
