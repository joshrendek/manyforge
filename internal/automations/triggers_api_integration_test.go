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
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/events"
)

func TestAutomationTriggersEventsEnrollmentsStatsAndGoldenScenario(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer database.Close(ctx)
	seed, foreign := seedTenant(ctx, t, database), seedTenant(ctx, t, database)
	mailingService := &mailing.Service{DB: database.App}
	service := &automations.Service{DB: database.App}
	list, err := mailingService.CreateList(ctx, seed.principalID, seed.businessID, mailing.ListInput{Name: "Golden list"})
	if err != nil {
		t.Fatal(err)
	}
	welcome, err := mailingService.CreateTemplate(ctx, seed.principalID, seed.businessID, mailing.TemplateInput{Name: "Welcome", Subject: "Welcome", BodyMarkdown: "Welcome"})
	if err != nil {
		t.Fatal(err)
	}
	reminder, err := mailingService.CreateTemplate(ctx, seed.principalID, seed.businessID, mailing.TemplateInput{Name: "Reminder", Subject: "Reminder", BodyMarkdown: "Reminder"})
	if err != nil {
		t.Fatal(err)
	}
	automationID, versionID := activateGoldenFlow(ctx, t, service, seed, list.ID, welcome.ID, reminder.ID)
	ports := mailing.AutomationPorts{MessageDomain: "mail.example.test"}
	deps := automations.Deps{Subscribers: ports, Sender: ports, Engagement: ports, Tagger: ports, Templates: ports, Lists: ports, Steps: automations.SQLStepStore{}}
	base := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)

	clicked, err := mailingService.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{Email: "clicked@example.test", SkipConfirmation: true, ConsentSource: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	trigger := automations.TriggerSubscriber{Subscribers: ports, Now: func() time.Time { return base }}
	handleSubscriberOutbox(ctx, t, database, trigger, clicked.ID, events.TopicMailingSubscriberActivated)
	clickedEnrollment := enrollmentFor(ctx, t, database, automationID, clicked.ID)
	stepper := &automations.Stepper{DB: database.App, Deps: deps, Now: func() time.Time { return base }}
	if err = stepper.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var clickedDelivery uuid.UUID
	if err = database.Super.QueryRow(ctx, `SELECT delivery_id FROM automation_enrollment_step
		WHERE enrollment_id=$1 AND node_id='welcome'`, clickedEnrollment).Scan(&clickedDelivery); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Super.Exec(ctx, `UPDATE mailing_delivery SET opened_at=$2 WHERE id=$1`, clickedDelivery, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Super.Exec(ctx, `INSERT INTO mailing_tracking_event
		(business_id,tenant_root_id,delivery_id,subscriber_id,kind,url,occurred_at)
		VALUES ($1,$1,$2,$3,'click','https://example.test/offer',$4)`, seed.businessID, clickedDelivery, clicked.ID, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stepper.Now = func() time.Time { return base.Add(24 * time.Hour) }
	if err = stepper.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	assertEnrollmentStatus(ctx, t, database, clickedEnrollment, "completed")
	var clickedTags int
	if err = database.Super.QueryRow(ctx, `SELECT count(*) FROM subscriber_tag WHERE subscriber_id=$1 AND tag='clicked'`, clicked.ID).Scan(&clickedTags); err != nil || clickedTags != 1 {
		t.Fatalf("clicked tag count=%d err=%v", clickedTags, err)
	}

	ignored, err := mailingService.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{Email: "ignored@example.test", SkipConfirmation: true, ConsentSource: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	trigger.Now = func() time.Time { return base.Add(48 * time.Hour) }
	handleSubscriberOutbox(ctx, t, database, trigger, ignored.ID, events.TopicMailingSubscriberActivated)
	ignoredEnrollment := enrollmentFor(ctx, t, database, automationID, ignored.ID)
	stepper.Now = func() time.Time { return base.Add(48 * time.Hour) }
	if err = stepper.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	stepper.Now = func() time.Time { return base.Add(72 * time.Hour) }
	if err = stepper.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	assertEnrollmentStatus(ctx, t, database, ignoredEnrollment, "completed")
	var reminders int
	if err = database.Super.QueryRow(ctx, `SELECT count(*) FROM automation_enrollment_step
		WHERE enrollment_id=$1 AND node_id='reminder' AND outcome='sent'`, ignoredEnrollment).Scan(&reminders); err != nil || reminders != 1 {
		t.Fatalf("reminder steps=%d err=%v", reminders, err)
	}

	manual, err := mailingService.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{Email: "manual@example.test", SkipConfirmation: true, ConsentSource: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	manualEnrollment, err := service.Enroll(ctx, seed.principalID, seed.businessID, automationID, manual.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Enroll(ctx, seed.principalID, seed.businessID, automationID, manual.ID); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("duplicate active enrollment error=%v", err)
	}
	foreignList, err := mailingService.CreateList(ctx, foreign.principalID, foreign.businessID, mailing.ListInput{Name: "Foreign"})
	if err != nil {
		t.Fatal(err)
	}
	foreignSubscriber, err := mailingService.CreateSubscriber(ctx, foreign.principalID, foreign.businessID, foreignList.ID, mailing.SubscriberInput{Email: "foreign@example.test", SkipConfirmation: true, ConsentSource: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Enroll(ctx, seed.principalID, seed.businessID, automationID, foreignSubscriber.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("foreign subscriber enrollment error=%v", err)
	}
	exited, err := service.ExitEnrollment(ctx, seed.principalID, seed.businessID, automationID, manualEnrollment.ID)
	if err != nil || exited.Status != "exited" || exited.ExitReason == nil || *exited.ExitReason != "manual" {
		t.Fatalf("manual exit=%+v err=%v", exited, err)
	}
	detail, err := service.GetEnrollment(ctx, seed.principalID, seed.businessID, automationID, clickedEnrollment)
	if err != nil || len(detail.Steps) != 6 {
		t.Fatalf("clicked detail steps=%d err=%v", len(detail.Steps), err)
	}
	page, err := service.ListEnrollments(ctx, seed.principalID, seed.businessID, automationID, automations.EnrollmentFilter{Status: "completed", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("completed enrollment page=%+v err=%v", page, err)
	}
	stats, err := service.Stats(ctx, seed.principalID, seed.businessID, automationID, &versionID)
	if err != nil || stats.Enrollments.Completed != 2 || stats.Enrollments.Exited != 1 || len(stats.Nodes) != 7 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	welcomeStats := statsNode(t, stats, "welcome")
	reminderStats := statsNode(t, stats, "reminder")
	if welcomeStats.Sent != 2 || welcomeStats.Opened != 1 || welcomeStats.Clicked != 1 {
		t.Fatalf("welcome node stats = %+v, want sent=2 opened=1 clicked=1", welcomeStats)
	}
	if reminderStats.Sent != 1 || reminderStats.Opened != 0 || reminderStats.Clicked != 0 {
		t.Fatalf("reminder node stats = %+v, want sent=1 opened=0 clicked=0", reminderStats)
	}

	eventAutomationID := activateEventFlow(ctx, t, service, seed, list.ID, "purchased")
	key := "purchase-1"
	clickedID := clicked.ID
	event, err := service.CreateEvent(ctx, seed.principalID, seed.businessID, automations.EventInput{Name: "purchased", SubscriberID: &clickedID, IdempotencyKey: &key, Properties: map[string]any{"plan": "pro"}})
	if err != nil || !event.Created {
		t.Fatalf("create event=%+v err=%v", event, err)
	}
	replayed, err := service.CreateEvent(ctx, seed.principalID, seed.businessID, automations.EventInput{Name: "purchased", SubscriberID: &clickedID, IdempotencyKey: &key})
	if err != nil || replayed.Created || replayed.ID != event.ID {
		t.Fatalf("replay event=%+v err=%v", replayed, err)
	}
	var eventOutbox int
	if err = database.Super.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE topic='automation.event.received' AND payload->>'event_id'=$1`, event.ID.String()).Scan(&eventOutbox); err != nil || eventOutbox != 1 {
		t.Fatalf("event outbox count=%d err=%v", eventOutbox, err)
	}
	handleAutomationEventOutbox(ctx, t, database, trigger, event.ID)
	_ = enrollmentFor(ctx, t, database, eventAutomationID, clicked.ID)

	tagAutomationID := activateTagFlow(ctx, t, service, seed, list.ID, "vip")
	tagged, err := mailingService.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{Email: "tagged@example.test", SkipConfirmation: true, ConsentSource: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	tags := []string{"vip"}
	if _, err = mailingService.UpdateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, tagged.ID, mailing.SubscriberUpdate{Tags: &tags}); err != nil {
		t.Fatal(err)
	}
	handleSubscriberOutbox(ctx, t, database, trigger, tagged.ID, events.TopicMailingSubscriberTagAdded)
	_ = enrollmentFor(ctx, t, database, tagAutomationID, tagged.ID)

	exitCandidate, err := mailingService.CreateSubscriber(ctx, seed.principalID, seed.businessID, list.ID, mailing.SubscriberInput{Email: "exit@example.test", SkipConfirmation: true, ConsentSource: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	exitEnrollment, err := service.Enroll(ctx, seed.principalID, seed.businessID, automationID, exitCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = mailingService.UnsubscribeSubscriber(ctx, seed.principalID, seed.businessID, list.ID, exitCandidate.ID, "requested"); err != nil {
		t.Fatal(err)
	}
	handleSubscriberOutbox(ctx, t, database, trigger, exitCandidate.ID, events.TopicMailingSubscriberStatusChanged)
	assertEnrollmentStatus(ctx, t, database, exitEnrollment.ID, "exited")

	badID := foreignSubscriber.ID
	if _, err = service.CreateEvent(ctx, seed.principalID, seed.businessID, automations.EventInput{Name: "purchased", SubscriberID: &badID}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("foreign event subscriber error=%v", err)
	}

	operationID := uuid.New()
	if _, err = database.Super.Exec(ctx, `INSERT INTO tenant_merge_operation
		(id,source_root_id,destination_parent_id,destination_root_id,actor_principal_id,idempotency_key,request_hash,status)
		VALUES ($1,$2,$3,$3,$4,$5,$6,'ready')`, operationID, seed.businessID, foreign.businessID,
		seed.principalID, "automation-fence-"+operationID.String(), []byte("request")); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Super.Exec(ctx, `INSERT INTO tenant_merge_fence (operation_id,root_id,root_role) VALUES ($1,$2,'source')`, operationID, seed.businessID); err != nil {
		t.Fatal(err)
	}
	var claimed int
	if err = database.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM automation_claim_due($1,100,interval '2 minutes')`, base.Add(7*24*time.Hour)).Scan(&claimed)
	}); err != nil || claimed != 0 {
		t.Fatalf("fenced-root automation claims=%d err=%v", claimed, err)
	}
}

func activateGoldenFlow(ctx context.Context, t *testing.T, service *automations.Service, seed tenantSeed, listID, welcomeID, reminderID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	created, err := service.Create(ctx, seed.principalID, seed.businessID, automations.CreateInput{Name: "Golden automation"})
	if err != nil {
		t.Fatal(err)
	}
	graph := automations.Graph{Nodes: []automations.Node{
		{ID: "trigger", Kind: "trigger", Config: json.RawMessage(`{"type":"list_joined","list_id":"` + listID.String() + `"}`)},
		{ID: "welcome", Kind: "send_email", Config: json.RawMessage(`{"template_id":"` + welcomeID.String() + `","track_opens":true,"track_clicks":true}`)},
		{ID: "wait", Kind: "wait", Config: json.RawMessage(`{"mode":"duration","seconds":86400}`)},
		{ID: "clicked", Kind: "condition", Config: json.RawMessage(`{"predicate":{"type":"clicked_link","node_id":"welcome"}}`)},
		{ID: "tag", Kind: "add_tag", Config: json.RawMessage(`{"tag":"clicked"}`)},
		{ID: "reminder", Kind: "send_email", Config: json.RawMessage(`{"template_id":"` + reminderID.String() + `","track_opens":true,"track_clicks":true}`)},
		{ID: "exit", Kind: "exit", Config: json.RawMessage(`{}`)},
	}, Edges: []automations.Edge{
		{ID: "e1", From: "trigger", To: "welcome"}, {ID: "e2", From: "welcome", To: "wait"},
		{ID: "e3", From: "wait", To: "clicked"}, {ID: "e4", From: "clicked", To: "tag", Branch: stringPtrTest("yes")},
		{ID: "e5", From: "clicked", To: "reminder", Branch: stringPtrTest("no")}, {ID: "e6", From: "tag", To: "exit"},
		{ID: "e7", From: "reminder", To: "exit"},
	}}
	if _, err = service.PutGraph(ctx, seed.principalID, seed.businessID, created.ID, *created.DraftVersionID, graph); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Activate(ctx, seed.principalID, seed.businessID, created.ID, *created.DraftVersionID); err != nil {
		t.Fatal(err)
	}
	return created.ID, *created.DraftVersionID
}

func activateEventFlow(ctx context.Context, t *testing.T, service *automations.Service, seed tenantSeed, listID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	created, err := service.Create(ctx, seed.principalID, seed.businessID, automations.CreateInput{Name: "Event automation"})
	if err != nil {
		t.Fatal(err)
	}
	graph := automations.Graph{Nodes: []automations.Node{
		{ID: "trigger", Kind: "trigger", Config: json.RawMessage(`{"type":"event","name":"` + name + `","list_id":"` + listID.String() + `"}`)},
		{ID: "exit", Kind: "exit", Config: json.RawMessage(`{}`)},
	}, Edges: []automations.Edge{{ID: "e1", From: "trigger", To: "exit"}}}
	if _, err = service.PutGraph(ctx, seed.principalID, seed.businessID, created.ID, *created.DraftVersionID, graph); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Activate(ctx, seed.principalID, seed.businessID, created.ID, *created.DraftVersionID); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func activateTagFlow(ctx context.Context, t *testing.T, service *automations.Service, seed tenantSeed, listID uuid.UUID, tag string) uuid.UUID {
	t.Helper()
	created, err := service.Create(ctx, seed.principalID, seed.businessID, automations.CreateInput{Name: "Tag automation"})
	if err != nil {
		t.Fatal(err)
	}
	graph := automations.Graph{Nodes: []automations.Node{
		{ID: "trigger", Kind: "trigger", Config: json.RawMessage(`{"type":"tag_added","tag":"` + tag + `","list_id":"` + listID.String() + `"}`)},
		{ID: "exit", Kind: "exit", Config: json.RawMessage(`{}`)},
	}, Edges: []automations.Edge{{ID: "e1", From: "trigger", To: "exit"}}}
	if _, err = service.PutGraph(ctx, seed.principalID, seed.businessID, created.ID, *created.DraftVersionID, graph); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Activate(ctx, seed.principalID, seed.businessID, created.ID, *created.DraftVersionID); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func handleSubscriberOutbox(ctx context.Context, t *testing.T, database *testdb.TestDB, trigger automations.TriggerSubscriber, subscriberID uuid.UUID, topic string) {
	t.Helper()
	var event events.Event
	if err := database.Super.QueryRow(ctx, `SELECT id,tenant_root_id,topic,payload,attempts FROM outbox
		WHERE topic=$1 AND payload->>'subscriber_id'=$2 ORDER BY created_at DESC,id DESC LIMIT 1`, topic, subscriberID.String()).Scan(&event.ID, &event.TenantRootID, &event.Topic, &event.Payload, &event.Attempts); err != nil {
		t.Fatal(err)
	}
	if err := database.App.WithTx(ctx, func(tx pgx.Tx) error { return trigger.Handle(ctx, tx, event) }); err != nil {
		t.Fatal(err)
	}
}

func handleAutomationEventOutbox(ctx context.Context, t *testing.T, database *testdb.TestDB, trigger automations.TriggerSubscriber, eventID uuid.UUID) {
	t.Helper()
	var event events.Event
	if err := database.Super.QueryRow(ctx, `SELECT id,tenant_root_id,topic,payload,attempts FROM outbox
		WHERE topic='automation.event.received' AND payload->>'event_id'=$1`, eventID.String()).Scan(&event.ID, &event.TenantRootID, &event.Topic, &event.Payload, &event.Attempts); err != nil {
		t.Fatal(err)
	}
	if err := database.App.WithTx(ctx, func(tx pgx.Tx) error { return trigger.Handle(ctx, tx, event) }); err != nil {
		t.Fatal(err)
	}
}

func enrollmentFor(ctx context.Context, t *testing.T, database *testdb.TestDB, automationID, subscriberID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := database.Super.QueryRow(ctx, `SELECT id FROM automation_enrollment WHERE automation_id=$1 AND subscriber_id=$2 ORDER BY enrolled_at DESC,id DESC LIMIT 1`, automationID, subscriberID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertEnrollmentStatus(ctx context.Context, t *testing.T, database *testdb.TestDB, enrollmentID uuid.UUID, want string) {
	t.Helper()
	var got string
	if err := database.Super.QueryRow(ctx, `SELECT status::text FROM automation_enrollment WHERE id=$1`, enrollmentID).Scan(&got); err != nil || got != want {
		t.Fatalf("enrollment status=%q want=%q err=%v", got, want, err)
	}
}

func statsNode(t *testing.T, stats automations.Stats, nodeID string) automations.NodeStats {
	t.Helper()
	for _, node := range stats.Nodes {
		if node.NodeID == nodeID {
			return node
		}
	}
	t.Fatalf("stats has no node %q", nodeID)
	return automations.NodeStats{}
}

func stringPtrTest(value string) *string { return &value }
