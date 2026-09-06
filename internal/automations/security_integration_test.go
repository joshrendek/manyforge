//go:build integration

package automations_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/automations"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/mailing"
	mailprovider "github.com/manyforge/manyforge/internal/mailing/provider"
	mailrender "github.com/manyforge/manyforge/internal/mailing/render"
	mailtoken "github.com/manyforge/manyforge/internal/mailing/token"
	"github.com/manyforge/manyforge/internal/platform/notify"
	"github.com/manyforge/manyforge/migrations"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestAutomationSendAuthz005ServiceRejectsWriteOnlyPrincipal(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer database.Close(ctx)

	seed := seedTenant(ctx, t, database)
	service := &automations.Service{DB: database.App}
	mailingService := &mailing.Service{DB: database.App}
	list, err := mailingService.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Authorized list"})
	if err != nil {
		t.Fatal(err)
	}
	template, err := mailingService.CreateTemplate(ctx, seed.principalID, seed.businessID, mailing.TemplateInput{
		Name: "Authorized template", Subject: "Original", BodyMarkdown: "Original body",
		TrackOpens: true, TrackClicks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := mailingService.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{
		Email: "write-only@example.test", SkipConfirmation: true, ConsentSource: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	automation, err := service.Create(ctx, seed.principalID, seed.businessID, automations.CreateInput{Name: "Authorized automation"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PutGraph(ctx, seed.principalID, seed.businessID, automation.ID, *automation.DraftVersionID, lifecycleGraph(list.ID, template.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Activate(ctx, seed.principalID, seed.businessID, automation.ID, *automation.DraftVersionID); err != nil {
		t.Fatal(err)
	}

	writeOnly := seedAutomationWriteOnlyPrincipal(ctx, t, database, seed.businessID)
	t.Run("manual enrollment", func(t *testing.T) {
		if _, err := service.Enroll(ctx, writeOnly, seed.businessID, automation.ID, subscriber.ID); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("Enroll error = %v, want uniform not found", err)
		}
		var count int
		if err := database.Super.QueryRow(ctx, `SELECT count(*) FROM automation_enrollment
			WHERE automation_id=$1 AND subscriber_id=$2`, automation.ID, subscriber.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("write-only principal created %d enrollments", count)
		}
	})

	t.Run("event injection", func(t *testing.T) {
		key := "write-only-event"
		email := subscriber.Email
		if _, err := service.CreateEvent(ctx, writeOnly, seed.businessID, automations.EventInput{
			Name: "write_only", Email: &email, IdempotencyKey: &key,
		}); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("CreateEvent error = %v, want uniform not found", err)
		}
		var count int
		if err := database.Super.QueryRow(ctx, `SELECT count(*) FROM automation_event
			WHERE business_id=$1 AND idempotency_key=$2`, seed.businessID, key).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("write-only principal created %d events", count)
		}
	})
}

func seedAutomationWriteOnlyPrincipal(ctx context.Context, t *testing.T, database *testdb.TestDB, businessID uuid.UUID) uuid.UUID {
	t.Helper()
	principalID, accountID, roleID := uuid.New(), uuid.New(), uuid.New()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at)
			VALUES ($1,$2,'Automation Writer','active',now(),now(),now())`, []any{accountID, "automation-writer-" + principalID.String() + "@x.test"}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{principalID, accountID}},
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at)
			VALUES ($1,$2,$3,'Automation Writer',false,now())`, []any{roleID, businessID, "automation-writer-" + roleID.String()}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES
			($1,'mailing.read'),($1,'mailing.write')`, []any{roleID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at)
			VALUES ($1,$2,$2,$3,now())`, []any{principalID, businessID, roleID}},
	}
	for _, statement := range statements {
		if _, err := database.Super.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed write-only principal: %v", err)
		}
	}
	return principalID
}

func TestMFAuto001ActivationValidatesTheVersionThatWinsTheRowLock(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer database.Close(ctx)

	seed := seedTenant(ctx, t, database)
	service := &automations.Service{DB: database.App}
	mailingService := &mailing.Service{DB: database.App}
	list, err := mailingService.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Activation lock list"})
	if err != nil {
		t.Fatal(err)
	}
	template, err := mailingService.CreateTemplate(ctx, seed.principalID, seed.businessID, mailing.TemplateInput{
		Name: "Activation lock template", Subject: "Original", BodyMarkdown: "Original body",
		TrackOpens: true, TrackClicks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	automation, err := service.Create(ctx, seed.principalID, seed.businessID, automations.CreateInput{Name: "Activation lock"})
	if err != nil {
		t.Fatal(err)
	}
	versionID := *automation.DraftVersionID
	if _, err = service.PutGraph(ctx, seed.principalID, seed.businessID, automation.ID, versionID, lifecycleGraph(list.ID, template.ID)); err != nil {
		t.Fatal(err)
	}

	blocker, err := database.Super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err = blocker.Exec(ctx, `SELECT 1 FROM automation_version WHERE id=$1 FOR UPDATE`, versionID); err != nil {
		t.Fatal(err)
	}

	putErr := make(chan error, 1)
	go func() {
		_, updateErr := service.PutGraph(ctx, seed.principalID, seed.businessID, automation.ID, versionID, automations.Graph{
			Nodes: []automations.Node{}, Edges: []automations.Edge{},
		})
		putErr <- updateErr
	}()
	waitForBlockedQuery(t, ctx, database, "UPDATE automation_version SET graph")

	activateErr := make(chan error, 1)
	go func() {
		_, activationErr := service.Activate(ctx, seed.principalID, seed.businessID, automation.ID, versionID)
		activateErr <- activationErr
	}()
	time.Sleep(50 * time.Millisecond)
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-putErr; err != nil {
		t.Fatalf("concurrent PutGraph: %v", err)
	}
	if err = <-activateErr; !isInvalidGraph(err) {
		t.Fatalf("Activate error = %v, want invalid graph after locked replacement", err)
	}

	var status string
	if err = database.Super.QueryRow(ctx, `SELECT status::text FROM automation WHERE id=$1`, automation.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "draft" {
		t.Fatalf("automation status = %q, want draft", status)
	}
}

func waitForBlockedQuery(t *testing.T, ctx context.Context, database *testdb.TestDB, fragment string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if err := database.Super.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND query LIKE '%' || $1 || '%'
			  AND wait_event_type = 'Lock'
		)`, fragment).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("query containing %q did not block", fragment)
}

func TestMFAuto002VersionRetentionIsBoundedAndPreservesReferencedVersions(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer database.Close(ctx)

	seed := seedTenant(ctx, t, database)
	service := &automations.Service{DB: database.App}
	mailingService := &mailing.Service{DB: database.App}
	list, err := mailingService.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Retention list"})
	if err != nil {
		t.Fatal(err)
	}
	template, err := mailingService.CreateTemplate(ctx, seed.principalID, seed.businessID, mailing.TemplateInput{
		Name: "Retention template", Subject: "Retention", BodyMarkdown: "Retention body",
		TrackOpens: true, TrackClicks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := mailingService.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{
		Email: "retention@example.test", SkipConfirmation: true, ConsentSource: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	automation, err := service.Create(ctx, seed.principalID, seed.businessID, automations.CreateInput{Name: "Retention"})
	if err != nil {
		t.Fatal(err)
	}
	referencedVersionID := *automation.DraftVersionID
	graph := lifecycleGraph(list.ID, template.ID)
	if _, err = service.PutGraph(ctx, seed.principalID, seed.businessID, automation.ID, referencedVersionID, graph); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Activate(ctx, seed.principalID, seed.businessID, automation.ID, referencedVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Enroll(ctx, seed.principalID, seed.businessID, automation.ID, subscriber.ID); err != nil {
		t.Fatal(err)
	}
	rawGraph, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Super.Exec(ctx, `UPDATE automation_version SET status='superseded' WHERE id=$1`, referencedVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Super.Exec(ctx, `INSERT INTO automation_version
		(id,business_id,tenant_root_id,automation_id,number,status,graph,trigger_kind,trigger_ref,activated_at,content_snapshot,created_at,updated_at)
		SELECT gen_random_uuid(),$1,$1,$2,n,
		       CASE WHEN n=100 THEN 'active'::automation_version_status ELSE 'superseded'::automation_version_status END,
		       $3::jsonb,'list_joined',$4,now(),
		       (SELECT content_snapshot FROM automation_version WHERE id=$5),now(),now()
		FROM generate_series(2,100) n`,
		seed.businessID, automation.ID, rawGraph, list.ID.String(), referencedVersionID); err != nil {
		t.Fatal(err)
	}
	var activeVersionID uuid.UUID
	if err = database.Super.QueryRow(ctx, `SELECT id FROM automation_version
		WHERE automation_id=$1 AND number=100`, automation.ID).Scan(&activeVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Super.Exec(ctx, `UPDATE automation SET active_version_id=$1 WHERE id=$2`,
		activeVersionID, automation.ID); err != nil {
		t.Fatal(err)
	}

	if _, err = service.CloneVersion(ctx, seed.principalID, seed.businessID, automation.ID); err != nil {
		t.Fatalf("CloneVersion at retention bound: %v", err)
	}
	var total int
	var referencedStillExists bool
	if err = database.Super.QueryRow(ctx, `SELECT count(*),bool_or(id=$2)
		FROM automation_version WHERE automation_id=$1`, automation.ID, referencedVersionID).
		Scan(&total, &referencedStillExists); err != nil {
		t.Fatal(err)
	}
	if total > 100 {
		t.Fatalf("retained versions = %d, want at most 100", total)
	}
	if !referencedStillExists {
		t.Fatal("retention deleted a version still referenced by an enrollment")
	}
	first, err := service.Versions(
		ctx, seed.principalID, seed.businessID, automation.ID, "", 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 10 || first.NextCursor == nil {
		t.Fatalf("first version page = %d items cursor=%v, want 10 items and cursor", len(first.Items), first.NextCursor)
	}
	second, err := service.Versions(
		ctx, seed.principalID, seed.businessID, automation.ID, *first.NextCursor, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 10 {
		t.Fatalf("second version page = %d items, want 10", len(second.Items))
	}
	firstIDs := make(map[uuid.UUID]struct{}, len(first.Items))
	for _, version := range first.Items {
		firstIDs[version.ID] = struct{}{}
	}
	for _, version := range second.Items {
		if _, duplicated := firstIDs[version.ID]; duplicated {
			t.Fatalf("version %s appeared on consecutive pages", version.ID)
		}
	}
}

func TestAutomationSecurityMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer database.Close(ctx)
	seed := seedTenant(ctx, t, database)
	mailingService := &mailing.Service{DB: database.App}
	list, err := mailingService.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Legacy migration"})
	if err != nil {
		t.Fatal(err)
	}
	template, err := mailingService.CreateTemplate(ctx, seed.principalID, seed.businessID, mailing.TemplateInput{
		Name: "Legacy migration", Subject: "Legacy", BodyMarkdown: "Legacy",
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := mailingService.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{
		Email: "legacy@example.test", SkipConfirmation: true, ConsentSource: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	down, err := migrations.FS.ReadFile("0135_automation_security.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Super.Exec(ctx, string(down)); err != nil {
		t.Fatalf("apply down migration: %v", err)
	}
	var legacyDeliveryID uuid.UUID
	if err = database.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT mailing_enqueue_delivery($1,$1,$2,$3,$4,now(),$5)`,
			seed.businessID, uuid.New(), template.ID, subscriber.ID, "mail.example.test").Scan(&legacyDeliveryID)
	}); err != nil {
		t.Fatalf("insert legacy automation delivery: %v", err)
	}
	up, err := migrations.FS.ReadFile("0135_automation_security.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.Super.Exec(ctx, string(up)); err != nil {
		t.Fatalf("reapply up migration: %v", err)
	}
	var legacyStatus string
	var legacyEnrollmentID *uuid.UUID
	if err = database.Super.QueryRow(ctx, `SELECT status::text,automation_enrollment_id
		FROM mailing_delivery WHERE id=$1`, legacyDeliveryID).Scan(&legacyStatus, &legacyEnrollmentID); err != nil {
		t.Fatal(err)
	}
	if legacyStatus != "cancelled" || legacyEnrollmentID != nil {
		t.Fatalf("legacy automation delivery status/fence = %q/%v, want cancelled/nil", legacyStatus, legacyEnrollmentID)
	}
}

type automationSecurityFixture struct {
	ctx             context.Context
	database        *testdb.TestDB
	seed            tenantSeed
	service         *automations.Service
	mailingService  *mailing.Service
	listID          uuid.UUID
	templateID      uuid.UUID
	subscriberID    uuid.UUID
	automationID    uuid.UUID
	versionID       uuid.UUID
	enrollmentID    uuid.UUID
	claimGeneration int
}

func newAutomationSecurityFixture(t *testing.T) automationSecurityFixture {
	t.Helper()
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { database.Close(ctx) })
	seed := seedTenant(ctx, t, database)
	service := &automations.Service{DB: database.App}
	mailingService := &mailing.Service{DB: database.App}
	list, err := mailingService.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Fence list"})
	if err != nil {
		t.Fatal(err)
	}
	template, err := mailingService.CreateTemplate(ctx, seed.principalID, seed.businessID, mailing.TemplateInput{
		Name: "Fence template", Subject: "Original subject", BodyMarkdown: "Original body",
		TrackOpens: true, TrackClicks: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := mailingService.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{
		Email: "fence@example.test", SkipConfirmation: true, ConsentSource: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	automation, err := service.Create(ctx, seed.principalID, seed.businessID, automations.CreateInput{Name: "Fence automation"})
	if err != nil {
		t.Fatal(err)
	}
	versionID := *automation.DraftVersionID
	if _, err = service.PutGraph(ctx, seed.principalID, seed.businessID, automation.ID, versionID, lifecycleGraph(list.ID, template.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Activate(ctx, seed.principalID, seed.businessID, automation.ID, versionID); err != nil {
		t.Fatal(err)
	}
	enrollment, err := service.Enroll(ctx, seed.principalID, seed.businessID, automation.ID, subscriber.ID)
	if err != nil {
		t.Fatal(err)
	}
	var claimedID uuid.UUID
	var generation int
	if err = database.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT enrollment_id,claim_generation
			FROM automation_claim_due(now(),1,interval '2 minutes')`).Scan(&claimedID, &generation)
	}); err != nil {
		t.Fatal(err)
	}
	if claimedID != enrollment.ID {
		t.Fatalf("claimed enrollment = %s, want %s", claimedID, enrollment.ID)
	}
	return automationSecurityFixture{
		ctx: ctx, database: database, seed: seed, service: service, mailingService: mailingService,
		listID: list.ID, templateID: template.ID, subscriberID: subscriber.ID,
		automationID: automation.ID, versionID: versionID, enrollmentID: enrollment.ID,
		claimGeneration: generation,
	}
}

func TestAutomationFence002LifecycleTransitionsFenceEverySideEffect(t *testing.T) {
	t.Run("pause fences enqueue", func(t *testing.T) {
		fixture := newAutomationSecurityFixture(t)
		if _, err := fixture.service.Pause(
			fixture.ctx, fixture.seed.principalID, fixture.seed.businessID, fixture.automationID,
		); err != nil {
			t.Fatal(err)
		}
		ports := mailing.AutomationPorts{MessageDomain: "mail.example.test"}
		err := fixture.database.App.WithTx(fixture.ctx, func(tx pgx.Tx) error {
			_, enqueueErr := ports.Enqueue(fixture.ctx, tx, automations.MessageSpec{
				BusinessID: fixture.seed.businessID, TenantRootID: fixture.seed.businessID,
				SubscriberID: fixture.subscriberID, TemplateID: fixture.templateID,
				EnrollmentID: fixture.enrollmentID, ClaimGeneration: fixture.claimGeneration,
				TrackOpens: true, TrackClicks: true, SourceKind: "automation",
				SourceID: uuid.New(), NotBefore: time.Now().UTC(),
			})
			return enqueueErr
		})
		if !errors.Is(err, automations.ErrLostFence) {
			t.Fatalf("enqueue after pause error = %v, want lost fence", err)
		}
		var deliveries int
		if err = fixture.database.Super.QueryRow(fixture.ctx, `SELECT count(*) FROM mailing_delivery
			WHERE source_kind='automation' AND subscriber_id=$1`, fixture.subscriberID).Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		if deliveries != 0 {
			t.Fatalf("deliveries after pause = %d, want 0", deliveries)
		}
	})

	t.Run("archive fences tag add", func(t *testing.T) {
		fixture := newAutomationSecurityFixture(t)
		if _, err := fixture.service.Archive(
			fixture.ctx, fixture.seed.principalID, fixture.seed.businessID, fixture.automationID,
		); err != nil {
			t.Fatal(err)
		}
		ports := mailing.AutomationPorts{}
		err := fixture.database.App.WithTx(fixture.ctx, func(tx pgx.Tx) error {
			return ports.AddTag(
				fixture.ctx, tx, fixture.seed.businessID, fixture.seed.businessID,
				fixture.subscriberID, fixture.enrollmentID, fixture.claimGeneration, "stale-add",
			)
		})
		if !errors.Is(err, automations.ErrLostFence) {
			t.Fatalf("tag add after archive error = %v, want lost fence", err)
		}
		var tags int
		if err = fixture.database.Super.QueryRow(fixture.ctx, `SELECT count(*) FROM subscriber_tag
			WHERE subscriber_id=$1 AND tag='stale-add'`, fixture.subscriberID).Scan(&tags); err != nil {
			t.Fatal(err)
		}
		if tags != 0 {
			t.Fatalf("tags after archive = %d, want 0", tags)
		}
	})

	t.Run("exit fences tag removal", func(t *testing.T) {
		fixture := newAutomationSecurityFixture(t)
		if _, err := fixture.database.Super.Exec(fixture.ctx, `INSERT INTO subscriber_tag
			(business_id,tenant_root_id,list_id,subscriber_id,tag)
			VALUES ($1,$1,$2,$3,'preserve-me')`,
			fixture.seed.businessID, fixture.listID, fixture.subscriberID); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.ExitEnrollment(
			fixture.ctx, fixture.seed.principalID, fixture.seed.businessID,
			fixture.automationID, fixture.enrollmentID,
		); err != nil {
			t.Fatal(err)
		}
		ports := mailing.AutomationPorts{}
		err := fixture.database.App.WithTx(fixture.ctx, func(tx pgx.Tx) error {
			return ports.RemoveTag(
				fixture.ctx, tx, fixture.seed.businessID, fixture.seed.businessID,
				fixture.subscriberID, fixture.enrollmentID, fixture.claimGeneration, "preserve-me",
			)
		})
		if !errors.Is(err, automations.ErrLostFence) {
			t.Fatalf("tag removal after exit error = %v, want lost fence", err)
		}
		var tags int
		if err = fixture.database.Super.QueryRow(fixture.ctx, `SELECT count(*) FROM subscriber_tag
			WHERE subscriber_id=$1 AND tag='preserve-me'`, fixture.subscriberID).Scan(&tags); err != nil {
			t.Fatal(err)
		}

		if tags != 1 {
			t.Fatalf("preserved tags after exit = %d, want 1", tags)
		}
	})

	t.Run("archived list fences enqueue", func(t *testing.T) {
		fixture := newAutomationSecurityFixture(t)
		if err := fixture.mailingService.ArchiveList(
			fixture.ctx, fixture.seed.principalID, fixture.seed.businessID, fixture.listID,
		); err != nil {
			t.Fatal(err)
		}
		ports := mailing.AutomationPorts{MessageDomain: "mail.example.test"}
		err := fixture.database.App.WithTx(fixture.ctx, func(tx pgx.Tx) error {
			_, enqueueErr := ports.Enqueue(fixture.ctx, tx, automations.MessageSpec{
				BusinessID: fixture.seed.businessID, TenantRootID: fixture.seed.businessID,
				SubscriberID: fixture.subscriberID, TemplateID: fixture.templateID,
				EnrollmentID: fixture.enrollmentID, ClaimGeneration: fixture.claimGeneration,
				TrackOpens: true, TrackClicks: true, SourceKind: "automation",
				SourceID: uuid.New(), NotBefore: time.Now().UTC(),
			})
			return enqueueErr
		})
		if !errors.Is(err, automations.ErrLostFence) {
			t.Fatalf("enqueue after list archive error = %v, want lost fence", err)
		}
	})
}

func TestMFAutoEnrollLifecycle006DelayedEventCannotEnrollAfterArchive(t *testing.T) {
	for _, archive := range []string{"mailing list", "business"} {
		t.Run(archive, func(t *testing.T) {
			fixture := newAutomationSecurityFixture(t)
			subscriber, err := fixture.mailingService.CreateSubscriber(
				fixture.ctx,
				fixture.seed.principalID,
				fixture.seed.businessID,
				fixture.listID,
				mailing.SubscriberInput{
					Email:            "delayed-" + uuid.NewString() + "@example.test",
					SkipConfirmation: true,
					ConsentSource:    "manual",
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			var sourceEventID uuid.UUID
			if err = fixture.database.Super.QueryRow(fixture.ctx, `
				SELECT id
				FROM outbox
				WHERE topic = 'mailing.subscriber.activated'
				  AND payload->>'subscriber_id' = $1::text
				ORDER BY created_at DESC
				LIMIT 1`,
				subscriber.ID,
			).Scan(&sourceEventID); err != nil {
				t.Fatal(err)
			}

			switch archive {
			case "mailing list":
				err = fixture.mailingService.ArchiveList(
					fixture.ctx,
					fixture.seed.principalID,
					fixture.seed.businessID,
					fixture.listID,
				)
			case "business":
				_, err = fixture.database.Super.Exec(fixture.ctx, `
					UPDATE business
					SET status = 'archived'
					WHERE id = $1`,
					fixture.seed.businessID)
			}
			if err != nil {
				t.Fatal(err)
			}

			var inserted int
			if err = fixture.database.App.WithTx(fixture.ctx, func(tx pgx.Tx) error {
				return tx.QueryRow(fixture.ctx, `
					SELECT automation_enroll_for_trigger(
						$1, $1, 'list_joined', $2, $3, $4, now()
					)`,
					fixture.seed.businessID,
					fixture.listID.String(),
					subscriber.ID,
					sourceEventID,
				).Scan(&inserted)
			}); err != nil {
				t.Fatal(err)
			}
			if inserted != 0 {
				t.Fatalf("enrollments created after %s archive = %d, want 0",
					archive, inserted)
			}

			var enrollments int
			if err = fixture.database.Super.QueryRow(fixture.ctx, `
				SELECT count(*)
				FROM automation_enrollment
				WHERE source_event_id = $1`,
				sourceEventID,
			).Scan(&enrollments); err != nil {
				t.Fatal(err)
			}
			if enrollments != 0 {
				t.Fatalf("stored enrollments after %s archive = %d, want 0",
					archive, enrollments)
			}

			if archive == "mailing list" {
				var definition string
				if err = fixture.database.Super.QueryRow(fixture.ctx, `
					SELECT pg_get_functiondef(
						'automation_enroll_for_trigger(uuid,uuid,text,text,uuid,uuid,timestamptz)'::regprocedure
					)`,
				).Scan(&definition); err != nil {
					t.Fatal(err)
				}
				for _, predicate := range []string{
					"mailing_business_operational",
					"mailing_list_operational",
					"v.content_snapshot IS NOT NULL",
				} {
					if !strings.Contains(definition, predicate) {
						t.Errorf("automation enrollment lifecycle guard omits %q", predicate)
					}
				}
			}
		})
	}
}

func TestAutomationFence002RenewalBlocksProviderAfterLifecycleTransition(t *testing.T) {
	tests := []struct {
		name       string
		transition func(automationSecurityFixture) error
	}{
		{name: "pause", transition: func(f automationSecurityFixture) error {
			_, err := f.service.Pause(f.ctx, f.seed.principalID, f.seed.businessID, f.automationID)
			return err
		}},
		{name: "automation archive", transition: func(f automationSecurityFixture) error {
			_, err := f.service.Archive(f.ctx, f.seed.principalID, f.seed.businessID, f.automationID)
			return err
		}},
		{name: "manual enrollment exit", transition: func(f automationSecurityFixture) error {
			_, err := f.service.ExitEnrollment(
				f.ctx, f.seed.principalID, f.seed.businessID, f.automationID, f.enrollmentID,
			)
			return err
		}},
		{name: "list archive", transition: func(f automationSecurityFixture) error {
			return f.mailingService.ArchiveList(f.ctx, f.seed.principalID, f.seed.businessID, f.listID)
		}},
		{name: "business archive", transition: func(f automationSecurityFixture) error {
			_, err := f.database.Super.Exec(f.ctx, `UPDATE business SET status='archived' WHERE id=$1`, f.seed.businessID)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newAutomationSecurityFixture(t)
			ports := mailing.AutomationPorts{MessageDomain: "mail.example.test"}
			sourceID := uuid.New()
			var deliveryID uuid.UUID
			if err := fixture.database.App.WithTx(fixture.ctx, func(tx pgx.Tx) error {
				var enqueueErr error
				deliveryID, enqueueErr = ports.Enqueue(fixture.ctx, tx, automations.MessageSpec{
					BusinessID: fixture.seed.businessID, TenantRootID: fixture.seed.businessID,
					SubscriberID: fixture.subscriberID, TemplateID: fixture.templateID,
					EnrollmentID: fixture.enrollmentID, ClaimGeneration: fixture.claimGeneration,
					TrackOpens: true, TrackClicks: true, SourceKind: "automation",
					SourceID: sourceID, NotBefore: time.Now().UTC(),
				})
				return enqueueErr
			}); err != nil {
				t.Fatal(err)
			}
			const deliveryGeneration = 7
			if _, err := fixture.database.Super.Exec(fixture.ctx, `UPDATE mailing_delivery
				SET status='sending',claim_generation=$2,lease_until=now()+interval '2 minutes'
				WHERE id=$1`, deliveryID, deliveryGeneration); err != nil {
				t.Fatal(err)
			}
			if err := tt.transition(fixture); err != nil {
				t.Fatal(err)
			}
			var renewed bool
			if err := fixture.database.App.WithTx(fixture.ctx, func(tx pgx.Tx) error {
				return tx.QueryRow(fixture.ctx,
					`SELECT mailing_renew_delivery($1,$2,interval '2 minutes')`,
					deliveryID, deliveryGeneration).Scan(&renewed)
			}); err != nil {
				t.Fatal(err)
			}
			providerCalls := 0
			if renewed {
				providerCalls++
			}
			if renewed || providerCalls != 0 {
				t.Fatalf("renewed=%v provider calls=%d, want fenced renewal and no provider send", renewed, providerCalls)
			}
		})
	}
}

type renewalCapturedDeliverer struct {
	calls int
}

func (d *renewalCapturedDeliverer) Verify(context.Context) error {
	return nil
}

func (d *renewalCapturedDeliverer) Send(context.Context, notify.Mail) (mailprovider.SendResult, error) {
	d.calls++
	return mailprovider.SendResult{ProviderID: "must-not-send"}, nil
}

func TestAutomationFence002SendWorkerDoesNotCallProviderAfterPause(t *testing.T) {
	fixture := newAutomationSecurityFixture(t)
	domainID, profileID := uuid.New(), uuid.New()
	if _, err := fixture.database.Super.Exec(fixture.ctx, `INSERT INTO email_domain
		(id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,created_at,updated_at)
		VALUES ($1,$2,$2,'renew.example.test','provider_route','verified',now(),now(),now())`,
		domainID, fixture.seed.businessID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.Super.Exec(fixture.ctx, `INSERT INTO mailing_sending_profile
		(id,business_id,tenant_root_id,mode,from_email,from_name,email_domain_id,status,
		 feedback_status,feedback_error,feedback_confirmed_at,created_at,updated_at)
		VALUES ($1,$2,$2,'relay','sender@renew.example.test','Sender',$3,'verified',
		        'ready',NULL,now(),now(),now())`,
		profileID, fixture.seed.businessID, domainID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.Super.Exec(fixture.ctx, `UPDATE automation_enrollment
		SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, fixture.enrollmentID); err != nil {
		t.Fatal(err)
	}
	ports := mailing.AutomationPorts{MessageDomain: "mail.example.test"}
	stepper := automations.Stepper{
		DB: fixture.database.App,
		Deps: automations.Deps{Subscribers: ports, Sender: ports, Steps: automations.SQLStepStore{}},
		Now: func() time.Time { return time.Now().UTC() },
	}
	if err := stepper.Tick(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	tokens, err := mailtoken.New(bytes.Repeat([]byte{0x54}, 32))
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := mailrender.New()
	if err != nil {
		t.Fatal(err)
	}
	captured := &renewalCapturedDeliverer{}
	paused := false
	sendService := &mailing.Service{
		DB: fixture.database.App, Tokens: tokens, Renderer: renderer,
		PublicBaseURL: "https://hub.example.test", MessageDomain: "mail.example.test",
		Providers: mailprovider.NewCache(func(context.Context, mailprovider.Profile) (mailprovider.Deliverer, error) {
			if !paused {
				paused = true
				if _, pauseErr := fixture.service.Pause(
					fixture.ctx, fixture.seed.principalID, fixture.seed.businessID, fixture.automationID,
				); pauseErr != nil {
					return nil, pauseErr
				}
			}
			return captured, nil
		}, time.Minute),
	}
	if err := (&mailing.SendWorker{Service: sendService, Batch: 1, Lease: 2 * time.Minute}).Tick(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if !paused {
		t.Fatal("provider resolution did not execute the pause transition")
	}
	if captured.calls != 0 {
		t.Fatalf("provider calls after pause = %d, want 0", captured.calls)
	}
}

func TestAutomationFence002DeliveryFenceIsImmutableAndConflictBound(t *testing.T) {
	fixture := newAutomationSecurityFixture(t)
	ports := mailing.AutomationPorts{MessageDomain: "mail.example.test"}
	sourceID := uuid.New()
	var deliveryID uuid.UUID
	if err := fixture.database.App.WithTx(fixture.ctx, func(tx pgx.Tx) error {
		var enqueueErr error
		deliveryID, enqueueErr = ports.Enqueue(fixture.ctx, tx, automations.MessageSpec{
			BusinessID: fixture.seed.businessID, TenantRootID: fixture.seed.businessID,
			SubscriberID: fixture.subscriberID, TemplateID: fixture.templateID,
			EnrollmentID: fixture.enrollmentID, ClaimGeneration: fixture.claimGeneration,
			TrackOpens: true, TrackClicks: true, SourceKind: "automation",
			SourceID: sourceID, NotBefore: time.Now().UTC(),
		})
		return enqueueErr
	}); err != nil {
		t.Fatal(err)
	}
	mutationErr := fixture.database.App.WithPrincipal(
		fixture.ctx, fixture.seed.principalID, func(tx pgx.Tx) error {
			_, err := tx.Exec(fixture.ctx, `UPDATE mailing_delivery
				SET automation_claim_generation=automation_claim_generation+1 WHERE id=$1`, deliveryID)
			return err
		},
	)
	if mutationErr == nil {
		t.Fatal("app role could mutate automation delivery fence columns after enqueue")
	}
	rawInsertErr := fixture.database.App.WithPrincipal(
		fixture.ctx, fixture.seed.principalID, func(tx pgx.Tx) error {
			_, err := tx.Exec(fixture.ctx, `INSERT INTO mailing_delivery
				(id,business_id,tenant_root_id,source_kind,source_id,template_id,subscriber_id,
				 email,not_before,message_id,automation_enrollment_id,automation_version_id,
				 automation_claim_generation)
				VALUES ($1,$2,$2,'automation',$3,$4,$5,'forged@example.test',now(),$6,$7,$8,$9)`,
				uuid.New(), fixture.seed.businessID, uuid.New(), fixture.templateID,
				fixture.subscriberID, uuid.NewString()+"@mail.example.test",
				fixture.enrollmentID, fixture.versionID, fixture.claimGeneration)
			return err
		},
	)
	if rawInsertErr == nil {
		t.Fatal("app role could forge a raw automation delivery")
	}
	if _, err := fixture.database.Super.Exec(fixture.ctx, `UPDATE automation_enrollment
		SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, fixture.enrollmentID); err != nil {
		t.Fatal(err)
	}
	var renewedGeneration int
	if err := fixture.database.App.WithTx(fixture.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(fixture.ctx, `SELECT claim_generation
			FROM automation_claim_due(now(),1,interval '2 minutes')`).Scan(&renewedGeneration)
	}); err != nil {
		t.Fatal(err)
	}
	err := fixture.database.App.WithTx(fixture.ctx, func(tx pgx.Tx) error {
		_, enqueueErr := ports.Enqueue(fixture.ctx, tx, automations.MessageSpec{
			BusinessID: fixture.seed.businessID, TenantRootID: fixture.seed.businessID,
			SubscriberID: fixture.subscriberID, TemplateID: fixture.templateID,
			EnrollmentID: fixture.enrollmentID, ClaimGeneration: renewedGeneration,
			TrackOpens: true, TrackClicks: true, SourceKind: "automation",
			SourceID: sourceID, NotBefore: time.Now().UTC(),
		})
		return enqueueErr
	})
	if !errors.Is(err, automations.ErrLostFence) {
		t.Fatalf("enqueue conflict with a different generation error = %v, want lost fence", err)
	}
	const deliveryGeneration = 11
	if _, err := fixture.database.Super.Exec(fixture.ctx, `UPDATE mailing_delivery
		SET status='sending',claim_generation=$2,lease_until=now()+interval '2 minutes'
		WHERE id=$1`, deliveryID, deliveryGeneration); err != nil {
		t.Fatal(err)
	}
	var renewed bool
	if err := fixture.database.App.WithTx(fixture.ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(fixture.ctx,
			`SELECT mailing_renew_delivery($1,$2,interval '2 minutes')`,
			deliveryID, deliveryGeneration).Scan(&renewed)
	}); err != nil {
		t.Fatal(err)
	}
	providerCalls := 0
	if renewed {
		providerCalls++
	}
	if renewed || providerCalls != 0 {
		t.Fatalf("mismatched delivery renewed=%v provider calls=%d, want no renewal or provider call", renewed, providerCalls)
	}
}

func TestAutomationSendAuthz005ActivationSnapshotFeedsDeliveryClaim(t *testing.T) {
	fixture := newAutomationSecurityFixture(t)
	mutatedSubject, mutatedBody := "Mutated subject", "Mutated body"
	if _, err := fixture.mailingService.UpdateTemplate(
		fixture.ctx, fixture.seed.principalID, fixture.seed.businessID, fixture.templateID,
		mailing.TemplateUpdate{Subject: &mutatedSubject, BodyMarkdown: &mutatedBody},
	); err != nil {
		t.Fatal(err)
	}
	domainID, profileID := uuid.New(), uuid.New()
	if _, err := fixture.database.Super.Exec(fixture.ctx, `INSERT INTO email_domain
		(id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,created_at,updated_at)
		VALUES ($1,$2,$2,'example.test','provider_route','verified',now(),now(),now())`,
		domainID, fixture.seed.businessID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.Super.Exec(fixture.ctx, `INSERT INTO mailing_sending_profile
		(id,business_id,tenant_root_id,mode,from_email,from_name,email_domain_id,status,
		 feedback_status,feedback_error,feedback_confirmed_at,created_at,updated_at)
		VALUES ($1,$2,$2,'relay','sender@example.test','Sender',$3,'verified',
		        'ready',NULL,now(),now(),now())`,
		profileID, fixture.seed.businessID, domainID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.Super.Exec(fixture.ctx, `UPDATE automation_enrollment
		SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, fixture.enrollmentID); err != nil {
		t.Fatal(err)
	}

	ports := mailing.AutomationPorts{MessageDomain: "mail.example.test"}
	stepper := automations.Stepper{
		DB: fixture.database.App,
		Deps: automations.Deps{Subscribers: ports, Sender: ports, Steps: automations.SQLStepStore{}},
		Now: func() time.Time { return time.Now().UTC() },
	}
	if err := stepper.Tick(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	var subject, body string
	if err := fixture.database.Super.QueryRow(fixture.ctx, `SELECT subject,body_markdown
		FROM mailing_claim_deliveries(1,interval '2 minutes')`).Scan(&subject, &body); err != nil {
		var status string
		var lastError *string
		_ = fixture.database.Super.QueryRow(fixture.ctx, `SELECT status::text,last_error
			FROM mailing_delivery WHERE subscriber_id=$1`, fixture.subscriberID).Scan(&status, &lastError)
		t.Fatalf("claim delivery: %v (status=%s last_error=%v)", err, status, lastError)
	}
	if subject != "Original subject" || body != "Original body" {
		t.Fatalf("claimed content = %q/%q, want immutable activation snapshot", subject, body)
	}
}
