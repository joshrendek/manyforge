//go:build integration

package automations_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/automations"
	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
)

func TestAutomationStepperAtomicDeliveryCrashReplay(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer database.Close(ctx)
	seed := seedTenant(ctx, t, database)
	mailingService := &mailing.Service{DB: database.App}
	list, err := mailingService.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Stepper list"})
	if err != nil {
		t.Fatal(err)
	}
	template, err := mailingService.CreateTemplate(ctx, seed.principalID, seed.businessID, mailing.TemplateInput{
		Name: "Stepper template", Subject: "Hello", BodyMarkdown: "Hello",
		TrackOpens: true, TrackClicks: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := mailingService.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{
		Email: "stepper@example.test", SkipConfirmation: true, ConsentSource: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	firstAutomation := activateStepperFlow(ctx, t, database, seed, list.ID, template.ID, "Atomic flow")
	firstEnrollment := enrollFlow(ctx, t, database, seed.businessID, list.ID, subscriber.ID, now)
	ports := mailing.AutomationPorts{MessageDomain: "mail.example.test"}
	deps := automations.Deps{
		Subscribers: ports, Sender: ports, Engagement: ports, Tagger: ports,
		Templates: ports, Lists: ports, Steps: automations.SQLStepStore{},
	}
	lockedTx, err := database.App.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var lockedID uuid.UUID
	if err := lockedTx.QueryRow(ctx, "SELECT enrollment_id FROM automation_claim_due($1,1,interval '2 minutes')", now).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	if lockedID != firstEnrollment {
		t.Fatalf("locked claim=%s want=%s", lockedID, firstEnrollment)
	}
	var skipped int
	if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM automation_claim_due($1,1,interval '2 minutes')", now).Scan(&skipped)
	}); err != nil {
		t.Fatal(err)
	}
	if skipped != 0 {
		t.Fatalf("SKIP LOCKED second claim count=%d want=0", skipped)
	}
	if err := lockedTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	stepper := &automations.Stepper{DB: database.App, Deps: deps, Now: func() time.Time { return now }}
	if err := stepper.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	assertCompletedFlow(ctx, t, database, firstAutomation, firstEnrollment, false, true)
	var firstDelivery uuid.UUID
	if err := database.Super.QueryRow(ctx, `SELECT delivery_id FROM automation_enrollment_step
		WHERE enrollment_id=$1 AND node_id='send'`, firstEnrollment).Scan(&firstDelivery); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Super.Exec(ctx, "UPDATE mailing_delivery SET opened_at=$2 WHERE id=$1", firstDelivery, now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Super.Exec(ctx, `INSERT INTO mailing_tracking_event
		(business_id,tenant_root_id,delivery_id,subscriber_id,kind,url,occurred_at)
		VALUES ($1,$1,$2,$3,'click','https://example.test/offer',$4)`, seed.businessID, firstDelivery, subscriber.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Super.Exec(ctx, `INSERT INTO automation_event
		(business_id,tenant_root_id,name,email,subscriber_id,occurred_at)
		VALUES ($1,$1,'checkout',$2,$3,$4)`, seed.businessID, subscriber.Email, subscriber.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
		snapshot, err := ports.Snapshot(ctx, tx, subscriber.ID)
		if err != nil {
			return err
		}
		active, err := ports.ActiveOnList(ctx, tx, seed.businessID, subscriber.Email, list.ID)
		if err != nil {
			return err
		}
		resolved, err := ports.ResolveForList(ctx, tx, seed.businessID, subscriber.Email, list.ID)
		if err != nil {
			return err
		}
		templateExists, err := ports.TemplateExists(ctx, tx, seed.businessID, seed.businessID, template.ID)
		if err != nil {
			return err
		}
		listExists, err := ports.ListExists(ctx, tx, seed.businessID, seed.businessID, list.ID)
		if err != nil {
			return err
		}
		engagement, err := ports.Engagement(ctx, tx, firstDelivery)
		if err != nil {
			return err
		}
		within := 5 * time.Minute
		eventExists, err := (automations.SQLStepStore{}).EventExists(ctx, tx, seed.businessID, subscriber.Email, "checkout", now.Add(-time.Hour), &within)
		if err != nil {
			return err
		}
		if snapshot.Status != "active" || !active || resolved != subscriber.ID || !templateExists || !listExists ||
			!engagement.Opened || len(engagement.ClickedURLs) != 1 || engagement.ClickedURLs[0] != "https://example.test/offer" || !eventExists {
			t.Fatalf("ports snapshot=%+v active=%t resolved=%s template=%t list=%t engagement=%+v event=%t",
				snapshot, active, resolved, templateExists, listExists, engagement, eventExists)
		}
		return nil
	}); err != nil {
		t.Fatalf("mailing automation ports: %v", err)
	}

	secondAutomation := activateStepperFlow(ctx, t, database, seed, list.ID, template.ID, "Crash flow")
	secondEnrollment := enrollFlow(ctx, t, database, seed.businessID, list.ID, subscriber.ID, now.Add(time.Second))
	crashing := deps
	crashing.Steps = failAfterSendStore{SQLStepStore: automations.SQLStepStore{}}
	crashStepper := &automations.Stepper{DB: database.App, Deps: crashing, Now: func() time.Time { return now.Add(time.Second) }}
	if err := crashStepper.Tick(ctx); err == nil {
		t.Fatal("crashing Tick unexpectedly succeeded")
	}
	var deliveries, steps int
	var current string
	var leased bool
	if err := database.Super.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM mailing_delivery WHERE source_kind='automation' AND subscriber_id=$1),
		(SELECT count(*) FROM automation_enrollment_step WHERE enrollment_id=$2),
		e.current_node_id,e.lease_expires_at IS NOT NULL
		FROM automation_enrollment e WHERE e.id=$2`, subscriber.ID, secondEnrollment).Scan(
		&deliveries, &steps, &current, &leased,
	); err != nil {
		t.Fatal(err)
	}
	if deliveries != 1 || steps != 0 || current != "trigger" || !leased {
		t.Fatalf("after crash deliveries=%d steps=%d current=%s leased=%t", deliveries, steps, current, leased)
	}
	if _, err := database.Super.Exec(ctx, "UPDATE automation_enrollment SET lease_expires_at=$2 WHERE id=$1", secondEnrollment, now); err != nil {
		t.Fatal(err)
	}
	replayStepper := &automations.Stepper{DB: database.App, Deps: deps, Now: func() time.Time { return now.Add(2 * time.Second) }}
	if err := replayStepper.Tick(ctx); err != nil {
		t.Fatalf("replay Tick: %v", err)
	}
	assertCompletedFlow(ctx, t, database, secondAutomation, secondEnrollment, false, true)
	var tagCount int
	if err := database.Super.QueryRow(ctx, "SELECT count(*) FROM subscriber_tag WHERE subscriber_id=$1 AND tag='engaged'", subscriber.ID).Scan(&tagCount); err != nil || tagCount != 1 {
		t.Fatalf("idempotent tag count=%d err=%v", tagCount, err)
	}
}

type failAfterSendStore struct{ automations.SQLStepStore }

func (s failAfterSendStore) Record(ctx context.Context, tx pgx.Tx, record automations.StepRecord) (bool, error) {
	changed, err := s.SQLStepStore.Record(ctx, tx, record)
	if err == nil && record.Outcome == "sent" {
		return false, errors.New("simulated crash after enqueue")
	}
	return changed, err
}

func activateStepperFlow(ctx context.Context, t *testing.T, database *testdb.TestDB, seed tenantSeed, listID, templateID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	service := &automations.Service{DB: database.App}
	created, err := service.Create(ctx, seed.principalID, seed.businessID, automations.CreateInput{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	graph := automations.Graph{Nodes: []automations.Node{
		{ID: "trigger", Kind: "trigger", Config: json.RawMessage(`{"type":"list_joined","list_id":"` + listID.String() + `"}`)},
		{ID: "send", Kind: "send_email", Config: json.RawMessage(`{"template_id":"` + templateID.String() + `","track_opens":false,"track_clicks":true}`)},
		{ID: "tag", Kind: "add_tag", Config: json.RawMessage(`{"tag":"engaged"}`)},
		{ID: "exit", Kind: "exit", Config: json.RawMessage(`{}`)},
	}, Edges: []automations.Edge{
		{ID: "e1", From: "trigger", To: "send"},
		{ID: "e2", From: "send", To: "tag"},
		{ID: "e3", From: "tag", To: "exit"},
	}}
	if _, err = service.PutGraph(ctx, seed.principalID, seed.businessID, created.ID, *created.DraftVersionID, graph); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Activate(ctx, seed.principalID, seed.businessID, created.ID, *created.DraftVersionID); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func enrollFlow(ctx context.Context, t *testing.T, database *testdb.TestDB, businessID, listID, subscriberID uuid.UUID, now time.Time) uuid.UUID {
	t.Helper()
	var inserted int
	if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT automation_enroll_for_trigger($1,$1,'list_joined',$2,$3,$4,$5)",
			businessID, listID.String(), subscriberID, uuid.New(), now).Scan(&inserted)
	}); err != nil {
		t.Fatal(err)
	}
	if inserted != 1 {
		t.Fatalf("enrolled flows=%d want=1", inserted)
	}
	var enrollmentID uuid.UUID
	if err := database.Super.QueryRow(ctx, `SELECT id FROM automation_enrollment
		WHERE business_id=$1 AND subscriber_id=$2 AND status='active'
		ORDER BY enrolled_at DESC,id DESC LIMIT 1`, businessID, subscriberID).Scan(&enrollmentID); err != nil {
		t.Fatal(err)
	}
	return enrollmentID
}

func assertCompletedFlow(ctx context.Context, t *testing.T, database *testdb.TestDB, automationID, enrollmentID uuid.UUID, trackOpens, trackClicks bool) {
	t.Helper()
	var status string
	var stepCount, deliveryCount int
	var opens, clicks bool
	if err := database.Super.QueryRow(ctx, `SELECT status::text FROM automation_enrollment WHERE id=$1 AND automation_id=$2`, enrollmentID, automationID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := database.Super.QueryRow(ctx, "SELECT count(*) FROM automation_enrollment_step WHERE enrollment_id=$1", enrollmentID).Scan(&stepCount); err != nil {
		t.Fatal(err)
	}
	if err := database.Super.QueryRow(ctx, `SELECT count(*),bool_and(d.track_opens_override),bool_and(d.track_clicks_override)
		FROM automation_enrollment_step s JOIN mailing_delivery d ON d.id=s.delivery_id
		WHERE s.enrollment_id=$1`, enrollmentID).Scan(&deliveryCount, &opens, &clicks); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || stepCount != 4 || deliveryCount != 1 || opens != trackOpens || clicks != trackClicks {
		t.Fatalf("flow status=%s steps=%d deliveries=%d tracking=%t/%t", status, stepCount, deliveryCount, opens, clicks)
	}
}
