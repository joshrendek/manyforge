//go:build integration

package automations_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/automations"
	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

type tenantSeed struct{ businessID, principalID uuid.UUID }

func seedTenant(ctx context.Context, t *testing.T, database *testdb.TestDB) tenantSeed {
	t.Helper()
	var ownerRole uuid.UUID
	if err := database.Super.QueryRow(ctx, "SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatal(err)
	}
	seed := tenantSeed{businessID: uuid.New(), principalID: uuid.New()}
	accountID := uuid.New()
	tx, err := database.Super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`, []any{accountID, "automation-" + seed.businessID.String() + "@x.test"}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`, []any{seed.principalID, accountID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'Automation Co','active',now(),now())`, []any{seed.businessID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`, []any{seed.businessID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`, []any{seed.principalID, seed.businessID, ownerRole}},
	}
	for _, statement := range statements {
		if _, err = tx.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return seed
}

func seedSibling(ctx context.Context, t *testing.T, database *testdb.TestDB, root uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := database.Super.Exec(ctx, `INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at)
		VALUES ($1,$2,$2,'Sibling','active',now(),now())`, id, root); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Super.Exec(ctx, `INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id)
		VALUES ($1,$1,0,$2),($2,$1,1,$2)`, id, root); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAutomationLifecycleAndIsolation(t *testing.T) {
	ctx := context.Background()
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	defer database.Close(ctx)
	a, b := seedTenant(ctx, t, database), seedTenant(ctx, t, database)
	sibling := seedSibling(ctx, t, database, a.businessID)
	service := &automations.Service{DB: database.App}

	created, err := service.Create(ctx, a.principalID, a.businessID, automations.CreateInput{Name: " Welcome flow ", AllowReenroll: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Name != "Welcome flow" || created.Status != "draft" || created.DraftVersionID == nil || created.ActiveVersionID != nil {
		t.Fatalf("created = %+v", created)
	}
	firstVersionID := *created.DraftVersionID
	versions, err := service.Versions(ctx, a.principalID, a.businessID, created.ID)
	if err != nil || len(versions) != 1 || versions[0].Number != 1 || versions[0].Status != "draft" {
		t.Fatalf("Versions = %+v, err=%v", versions, err)
	}
	invalid, err := service.ValidateVersion(ctx, a.principalID, a.businessID, created.ID, firstVersionID)
	if err != nil || invalid.Valid || len(invalid.Issues) == 0 {
		t.Fatalf("ValidateVersion empty = %+v, err=%v", invalid, err)
	}
	if _, err = service.Activate(ctx, a.principalID, a.businessID, created.ID, firstVersionID); !isInvalidGraph(err) {
		t.Fatalf("Activate invalid error = %v", err)
	}

	mailingService := &mailing.Service{DB: database.App}
	list, err := mailingService.CreateList(ctx, a.principalID, a.businessID, mailing.ListInput{Name: "Customers"})
	if err != nil {
		t.Fatal(err)
	}
	template, err := mailingService.CreateTemplate(ctx, a.principalID, a.businessID, mailing.TemplateInput{Name: "Welcome", Subject: "Welcome", BodyMarkdown: "Hello", TrackOpens: true, TrackClicks: true})
	if err != nil {
		t.Fatal(err)
	}
	foreignList, err := mailingService.CreateList(ctx, b.principalID, b.businessID, mailing.ListInput{Name: "Foreign customers"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.PutGraph(ctx, a.principalID, a.businessID, created.ID, firstVersionID, lifecycleGraph(foreignList.ID, template.ID)); err != nil {
		t.Fatal(err)
	}
	foreignValidation, err := service.ValidateVersion(ctx, a.principalID, a.businessID, created.ID, firstVersionID)
	if err != nil || foreignValidation.Valid || !hasIssue(foreignValidation.Issues, "list_not_found") {
		t.Fatalf("foreign reference validation = %+v, err=%v", foreignValidation, err)
	}
	graph := lifecycleGraph(list.ID, template.ID)
	version, err := service.PutGraph(ctx, a.principalID, a.businessID, created.ID, firstVersionID, graph)
	if err != nil || len(version.Graph.Nodes) != 3 {
		t.Fatalf("PutGraph = %+v, err=%v", version, err)
	}
	valid, err := service.ValidateVersion(ctx, a.principalID, a.businessID, created.ID, firstVersionID)
	if err != nil || !valid.Valid || len(valid.Issues) != 0 {
		t.Fatalf("ValidateVersion = %+v, err=%v", valid, err)
	}
	active, err := service.Activate(ctx, a.principalID, a.businessID, created.ID, firstVersionID)
	if err != nil || active.Status != "active" || active.ActiveVersionID == nil || *active.ActiveVersionID != firstVersionID || active.DraftVersionID != nil {
		t.Fatalf("Activate = %+v, err=%v", active, err)
	}
	if _, err = service.PutGraph(ctx, a.principalID, a.businessID, created.ID, firstVersionID, graph); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("PutGraph active error = %v", err)
	}
	if _, err = service.PutGraph(ctx, a.principalID, a.businessID, created.ID, uuid.New(), graph); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("PutGraph unknown version error = %v", err)
	}
	if _, err = service.Activate(ctx, a.principalID, a.businessID, created.ID, uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("Activate unknown version error = %v", err)
	}

	draft, err := service.CloneVersion(ctx, a.principalID, a.businessID, created.ID)
	if err != nil || draft.Number != 2 || draft.Status != "draft" || len(draft.Graph.Nodes) != len(graph.Nodes) {
		t.Fatalf("CloneVersion = %+v, err=%v", draft, err)
	}
	if _, err = service.CloneVersion(ctx, a.principalID, a.businessID, created.ID); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("second CloneVersion error = %v", err)
	}
	active, err = service.Activate(ctx, a.principalID, a.businessID, created.ID, draft.ID)
	if err != nil || active.ActiveVersionID == nil || *active.ActiveVersionID != draft.ID {
		t.Fatalf("Activate v2 = %+v, err=%v", active, err)
	}
	versions, err = service.Versions(ctx, a.principalID, a.businessID, created.ID)
	if err != nil || versions[0].Status != "active" || versions[1].Status != "superseded" {
		t.Fatalf("version lifecycle = %+v, err=%v", versions, err)
	}
	paused, err := service.Pause(ctx, a.principalID, a.businessID, created.ID)
	if err != nil || paused.Status != "paused" {
		t.Fatalf("Pause = %+v, err=%v", paused, err)
	}
	resumed, err := service.Resume(ctx, a.principalID, a.businessID, created.ID)
	if err != nil || resumed.Status != "active" {
		t.Fatalf("Resume = %+v, err=%v", resumed, err)
	}

	subscriber, err := mailingService.CreateSubscriber(ctx, a.principalID, a.businessID, list.ID, mailing.SubscriberInput{Email: "flow@example.test", SkipConfirmation: true, ConsentSource: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	enrollmentID := uuid.New()
	if _, err = database.Super.Exec(ctx, `INSERT INTO automation_enrollment
		(id,business_id,tenant_root_id,automation_id,version_id,subscriber_id,status,current_node_id,wake_at,enrolled_at,updated_at)
		VALUES ($1,$2,$2,$3,$4,$5,'active','trigger',now(),now(),now())`, enrollmentID, a.businessID, created.ID, draft.ID, subscriber.ID); err != nil {
		t.Fatal(err)
	}
	archived, err := service.Archive(ctx, a.principalID, a.businessID, created.ID)
	if err != nil || archived.Status != "archived" || archived.DraftVersionID != nil {
		t.Fatalf("Archive = %+v, err=%v", archived, err)
	}
	var enrollmentStatus, exitReason string
	var finished bool
	if err = database.Super.QueryRow(ctx, `SELECT status::text,exit_reason,finished_at IS NOT NULL FROM automation_enrollment WHERE id=$1`, enrollmentID).Scan(&enrollmentStatus, &exitReason, &finished); err != nil || enrollmentStatus != "exited" || exitReason != "archived" || !finished {
		t.Fatalf("archived enrollment = %s/%s/%t, err=%v", enrollmentStatus, exitReason, finished, err)
	}

	if _, err = service.Get(ctx, b.principalID, b.businessID, created.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Get error = %v", err)
	}
	if _, err = service.Get(ctx, a.principalID, sibling, created.ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("same-root sibling Get error = %v", err)
	}
	if _, err = service.Version(ctx, b.principalID, b.businessID, created.ID, firstVersionID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("cross-tenant Version error = %v", err)
	}
	name := "cannot edit"
	if _, err = service.Update(ctx, a.principalID, a.businessID, created.ID, automations.UpdateInput{Name: &name}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("Update archived error = %v", err)
	}

	second, err := service.Create(ctx, a.principalID, a.businessID, automations.CreateInput{Name: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.List(ctx, a.principalID, a.businessID, "", 1)
	if err != nil || len(page.Items) != 1 || page.NextCursor == nil || page.Items[0].ID != second.ID {
		t.Fatalf("first page = %+v, err=%v", page, err)
	}
	next, err := service.List(ctx, a.principalID, a.businessID, *page.NextCursor, 1)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != created.ID {
		t.Fatalf("next page = %+v, err=%v", next, err)
	}

	var audits int
	if err = database.Super.QueryRow(ctx, `SELECT count(*) FROM audit_entry WHERE business_id=$1 AND action LIKE 'automation.%'`, a.businessID).Scan(&audits); err != nil || audits < 8 {
		t.Fatalf("audit count = %d, err=%v", audits, err)
	}
}

func lifecycleGraph(listID, templateID uuid.UUID) automations.Graph {
	return automations.Graph{
		Nodes: []automations.Node{
			{ID: "trigger", Kind: "trigger", Config: json.RawMessage(`{"type":"list_joined","list_id":"` + listID.String() + `"}`)},
			{ID: "welcome", Kind: "send_email", Config: json.RawMessage(`{"template_id":"` + templateID.String() + `","track_opens":true,"track_clicks":true}`)},
			{ID: "exit", Kind: "exit", Config: json.RawMessage(`{}`)},
		},
		Edges: []automations.Edge{{ID: "e1", From: "trigger", To: "welcome"}, {ID: "e2", From: "welcome", To: "exit"}},
	}
}

func isInvalidGraph(err error) bool {
	var invalid *automations.InvalidGraphError
	return errors.As(err, &invalid)
}

func hasIssue(issues []automations.Issue, code string) bool {
	for _, issue := range issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}
