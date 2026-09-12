//go:build integration

package mailing_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/mailing"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/platform/timeseries"
	"github.com/manyforge/manyforge/internal/tenancy"
	"github.com/manyforge/manyforge/migrations"
)

func reportingDB(t *testing.T) (context.Context, *testdb.TestDB, *mailing.Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	database, err := testdb.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(context.Background()) })
	return ctx, database, &mailing.Service{DB: database.App}
}

func reportingExec(t *testing.T, ctx context.Context, database *testdb.TestDB, query string, args ...any) {
	t.Helper()
	if _, err := database.Super.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func reportingList(t *testing.T, ctx context.Context, svc *mailing.Service, principal, business uuid.UUID, name string) mailing.List {
	t.Helper()
	list, err := svc.CreateList(ctx, principal, business, mailing.ListInput{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func reportingSubscriber(t *testing.T, ctx context.Context, svc *mailing.Service, principal uuid.UUID, list mailing.List, email string) mailing.Subscriber {
	t.Helper()
	subscriber, err := svc.CreateSubscriber(ctx, principal, list.BusinessID, list.ID, mailing.SubscriberInput{
		Email: email, ConsentSource: "manual", SkipConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return subscriber
}

func reportingBalance(t *testing.T, ctx context.Context, svc *mailing.Service, principal uuid.UUID, business *uuid.UUID, active, net int64) mailing.MailingReport {
	t.Helper()
	report, err := svc.Reporting(ctx, principal, business)
	if err != nil {
		t.Fatal(err)
	}
	if report.ActiveSubscribers != active || report.SubscriberNetAdditions != net {
		t.Fatalf("subscriber balance = active %d, net %d; want %d, %d", report.ActiveSubscribers, report.SubscriberNetAdditions, active, net)
	}
	return report
}

func reportingRate(t *testing.T, name string, rate mailing.MailingReportRate, numerator, denominator int64) {
	t.Helper()
	if rate.Numerator != numerator || rate.Denominator != denominator {
		t.Fatalf("%s = %+v, want %d/%d", name, rate, numerator, denominator)
	}
	if denominator == 0 {
		if rate.Percent != nil {
			t.Fatalf("%s undefined percentage = %v", name, *rate.Percent)
		}
	} else if rate.Percent == nil || math.Abs(*rate.Percent-100*float64(numerator)/float64(denominator)) > 1e-10 {
		t.Fatalf("%s weighted percentage = %v", name, rate.Percent)
	}
}

func reportingHTTP(t *testing.T, svc *mailing.Service) (*httptest.Server, *auth.KeyRing) {
	t.Helper()
	ring, err := auth.NewDevKeyRing("report-test", "report-api")
	if err != nil {
		t.Fatal(err)
	}
	router := httpx.NewRouter(ring)
	router.Route("/api/v1", func(r chi.Router) {
		r.Use(httpx.RequireAuth)
		mailing.NewHandler(svc).ReportingRoutes(r)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, ring
}

func reportingGet(t *testing.T, server *httptest.Server, ring *auth.KeyRing, principal uuid.UUID, query string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/mailing/reporting"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if principal != uuid.Nil {
		token, err := ring.Sign(principal, time.Minute, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func TestMailingReportingHTTPAuthorizationDedupAndWeightedCohorts(t *testing.T) {
	ctx, database, svc := reportingDB(t)
	a := seedMailingTenant(ctx, t, database)
	b := seedMailingTenant(ctx, t, database)
	foreign := seedMailingTenant(ctx, t, database)
	left := seedSibling(ctx, t, database, a.businessID)
	right := seedSibling(ctx, t, database, a.businessID)
	inactive := seedSibling(ctx, t, database, a.businessID)
	reader := seedMailingWriteOnlyPrincipal(ctx, t, database, a.businessID)
	// Membership alone is not mailing.read, and a child grant does not authorize
	// the hidden parent. The same principal also has a grant in a second tenant.
	reportingExec(t, ctx, database, `UPDATE membership SET business_id=$1 WHERE principal_id=$2`, left, reader)
	reportingExec(t, ctx, database, `INSERT INTO membership(principal_id,business_id,tenant_root_id,role_id)
		SELECT principal_id,$1,tenant_root_id,role_id FROM membership WHERE principal_id=$2 AND business_id=$3`, right, reader, left)
	reportingExec(t, ctx, database, `INSERT INTO membership(principal_id,business_id,tenant_root_id,role_id)
		SELECT principal_id,$1,tenant_root_id,role_id FROM membership WHERE principal_id=$2 AND business_id=$3`, inactive, reader, left)
	reportingExec(t, ctx, database, `INSERT INTO membership(principal_id,business_id,tenant_root_id,role_id)
		SELECT $1,$2,$2,id FROM role WHERE tenant_root_id IS NULL AND key='owner'`, reader, b.businessID)

	leftList := reportingList(t, ctx, svc, a.principalID, left, "Left")
	leftDuplicate := reportingList(t, ctx, svc, a.principalID, left, "Duplicate")
	rightList := reportingList(t, ctx, svc, a.principalID, right, "Right")
	bList := reportingList(t, ctx, svc, b.principalID, b.businessID, "Other tenant")
	leftSub := reportingSubscriber(t, ctx, svc, a.principalID, leftList, "shared@example.test")
	_ = reportingSubscriber(t, ctx, svc, a.principalID, leftDuplicate, "SHARED@EXAMPLE.TEST")
	rightSub := reportingSubscriber(t, ctx, svc, a.principalID, rightList, "shared@EXAMPLE.test")
	bSub := reportingSubscriber(t, ctx, svc, b.principalID, bList, "SHARED@example.test")
	// Exceed every mailing/automation list cap, without relying on a capped API
	// to manufacture totals. Distinct lists defend accidental list pagination too.
	reportingExec(t, ctx, database, `WITH lists AS (
		INSERT INTO mailing_list(business_id,tenant_root_id,name,slug)
		SELECT $1,$2,'Bulk '||n,'bulk-'||n FROM generate_series(1,231) n RETURNING id,slug
	) INSERT INTO list_subscriber(business_id,tenant_root_id,list_id,email,status,consent_source,consent_attested_by)
		SELECT $1,$2,id,slug||'@example.test','active','manual',$3 FROM lists`, left, a.businessID, a.principalID)
	reportingExec(t, ctx, database, `INSERT INTO automation(business_id,tenant_root_id,name,status)
		SELECT $1,$2,'Bulk '||n,'active' FROM generate_series(1,231) n`, left, a.businessID)
	var paused, version uuid.UUID
	if err := database.Super.QueryRow(ctx, `INSERT INTO automation(business_id,tenant_root_id,name,status)
		VALUES($1,$1,'Paused enrollment owner','paused') RETURNING id`, b.businessID).Scan(&paused); err != nil {
		t.Fatal(err)
	}
	if err := database.Super.QueryRow(ctx, `INSERT INTO automation_version(business_id,tenant_root_id,automation_id,number)
		VALUES($1,$1,$2,1) RETURNING id`, b.businessID, paused).Scan(&version); err != nil {
		t.Fatal(err)
	}
	reportingExec(t, ctx, database, `INSERT INTO automation_enrollment(business_id,tenant_root_id,automation_id,version_id,subscriber_id)
		VALUES($1,$1,$2,$3,$4)`, b.businessID, paused, version, bSub.ID)

	seedCampaign := func(list mailing.List, sub mailing.Subscriber, messages, opens, clicks int) uuid.UUID {
		t.Helper()
		campaignID := uuid.New()
		reportingExec(t, ctx, database, `INSERT INTO campaign(id,business_id,tenant_root_id,list_id,name,subject,body_markdown,track_opens,track_clicks)
			VALUES($1,$2,$3,$4,'Report cohort','Subject','Body',true,true)`, campaignID, list.BusinessID, list.TenantRootID, list.ID)
		reportingExec(t, ctx, database, `INSERT INTO mailing_delivery
			(business_id,tenant_root_id,source_kind,source_id,campaign_id,subscriber_id,email,status,message_id,opened_at,first_clicked_at,created_at)
			SELECT $1,$2,'campaign',gen_random_uuid(),$3,$4,$5,
			       (ARRAY['sent','delivered','bounced','complained'])[1+(n-1)%4]::mailing_delivery_status,
			       gen_random_uuid()::text,CASE WHEN n<=$7 THEN now() END,CASE WHEN n<=$8 THEN now() END,now()-interval '1 hour'
			FROM generate_series(1,$6::int) n`, list.BusinessID, list.TenantRootID, campaignID, sub.ID, sub.Email, messages, opens, clicks)
		return campaignID
	}
	campaignID := seedCampaign(leftList, leftSub, 9, 1, 1)
	_ = seedCampaign(rightList, rightSub, 1, 1, 0)
	var templateID uuid.UUID
	if err := database.Super.QueryRow(ctx, `INSERT INTO mailing_template(business_id,tenant_root_id,name,subject,body_markdown)
		VALUES($1,$1,'Snapshot source','Subject','Body') RETURNING id`, b.businessID).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	reportingExec(t, ctx, database, `INSERT INTO mailing_delivery
		(business_id,tenant_root_id,source_kind,source_id,template_id,subscriber_id,email,status,message_id,
		 track_opens_override,track_clicks_override,opened_at,first_clicked_at,created_at)
		SELECT $1,$1,'automation',gen_random_uuid(),$2,$3,$4,'sent',gen_random_uuid()::text,
		       false,true,now(),CASE WHEN n=1 THEN now() ELSE now()+interval '1 day' END,now()-interval '1 hour'
		FROM generate_series(1,2) n`, b.businessID, templateID, bSub.ID, bSub.Email)
	// Untracked and repeated observations must not inflate message numerators.
	reportingExec(t, ctx, database, `INSERT INTO mailing_tracking_event(business_id,tenant_root_id,delivery_id,kind)
		SELECT d.business_id,d.tenant_root_id,d.id,'unsubscribe'
		FROM (SELECT DISTINCT ON (business_id) * FROM mailing_delivery WHERE business_id IN ($1,$2) ORDER BY business_id,id) d
		CROSS JOIN generate_series(1,3) n`, left, b.businessID)
	// All nonaccepted statuses, old/future queued cohorts, and hidden scopes
	// carry observations that would contaminate a broadly filtered report.
	reportingExec(t, ctx, database, `INSERT INTO mailing_delivery
		(business_id,tenant_root_id,source_kind,source_id,campaign_id,subscriber_id,email,status,message_id,opened_at,created_at)
		SELECT $1,$2,'campaign',gen_random_uuid(),$3,$4,$5,status::mailing_delivery_status,gen_random_uuid()::text,now(),now()-interval '1 hour'
		FROM unnest(ARRAY['queued','sending','failed','suppressed','cancelled']) status`, left, a.businessID, campaignID, leftSub.ID, leftSub.Email)
	reportingExec(t, ctx, database, `INSERT INTO mailing_delivery
		(business_id,tenant_root_id,source_kind,source_id,campaign_id,subscriber_id,email,status,message_id,opened_at,created_at)
		SELECT $1,$2,'campaign',gen_random_uuid(),$3,$4,$5,'sent',gen_random_uuid()::text,now(),now()+age
		FROM unnest(ARRAY[interval '-8 days',interval '1 day']) age`, left, a.businessID, campaignID, leftSub.ID, leftSub.Email)
	for _, hidden := range []struct{ owner, business uuid.UUID }{
		{a.principalID, a.businessID}, {foreign.principalID, foreign.businessID}, {a.principalID, inactive},
	} {
		list := reportingList(t, ctx, svc, hidden.owner, hidden.business, "Hidden")
		sub := reportingSubscriber(t, ctx, svc, hidden.owner, list, "hidden-"+hidden.business.String()+"@example.test")
		_ = seedCampaign(list, sub, 11, 11, 11)
		reportingExec(t, ctx, database, `INSERT INTO automation(business_id,tenant_root_id,name,status) VALUES($1,$2,'Hidden','active')`, list.BusinessID, list.TenantRootID)
	}
	reportingExec(t, ctx, database, `UPDATE business SET status='archived' WHERE id=$1`, inactive)

	server, ring := reportingHTTP(t, svc)
	status, body := reportingGet(t, server, ring, reader, "")
	if status != http.StatusOK {
		t.Fatalf("HTTP report %d: %s", status, body)
	}
	var report mailing.MailingReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	if report.BusinessCount != 3 || report.TenantCount != 2 || report.ActiveSubscribers != 233 ||
		report.SubscriberNetAdditions != 233 || report.ActiveAutomations != 231 || report.ActiveEnrollments != 1 {
		t.Fatalf("complete authorized totals = %+v", report)
	}
	if report.AsOf.Sub(report.WindowStart) != 7*24*time.Hour || report.SubscriberWindowComplete ||
		!report.SubscriberWindowStart.After(report.WindowStart) {
		t.Fatalf("initial history honesty = %+v", report)
	}
	reportingRate(t, "opens", report.OpenRate, 2, 10)
	reportingRate(t, "clicks", report.ClickRate, 2, 12)
	reportingRate(t, "unsubscribes", report.UnsubscribeRate, 2, 12)
	for _, pii := range []string{"example.test", leftSub.ID.String(), leftList.ID.String(), a.businessID.String()} {
		if strings.Contains(string(body), pii) {
			t.Fatalf("report leaked source identity %q", pii)
		}
	}
	status, body = reportingGet(t, server, ring, reader, "?business_id="+right.String())
	if status != http.StatusOK || json.Unmarshal(body, &report) != nil {
		t.Fatalf("filtered report %d: %s", status, body)
	}
	if report.BusinessCount != 1 || report.TenantCount != 1 || report.ActiveSubscribers != 1 {
		t.Fatalf("filtered totals = %+v", report)
	}
	reportingRate(t, "filtered opens", report.OpenRate, 1, 1)

	var notFound string
	for _, query := range []string{a.businessID.String(), foreign.businessID.String(), inactive.String(), uuid.New().String(), "invalid", ""} {
		status, body = reportingGet(t, server, ring, reader, "?business_id="+query)
		if status != http.StatusNotFound {
			t.Fatalf("uniform not-found (%s) = %d: %s", query, status, body)
		}
		if notFound != "" && string(body) != notFound {
			t.Fatalf("scope oracle: %q != %q", body, notFound)
		}
		notFound = string(body)
	}
	if status, _ := reportingGet(t, server, ring, uuid.Nil, ""); status != http.StatusUnauthorized {
		t.Fatalf("anonymous report status = %d", status)
	}
	// Revocation is applied afresh, not cached from the previous report.
	reportingExec(t, ctx, database, `DELETE FROM role_permission WHERE permission_key='mailing.read'
		AND role_id IN (SELECT role_id FROM membership WHERE principal_id=$1 AND business_id=$2)`, reader, left)
	if status, _ := reportingGet(t, server, ring, reader, "?business_id="+left.String()); status != http.StatusNotFound {
		t.Fatalf("revoked report status = %d", status)
	}
}

func TestMailingReportingZeroCohortsAndKnownTracking(t *testing.T) {
	ctx, database, svc := reportingDB(t)
	seed := seedMailingTenant(ctx, t, database)
	report := reportingBalance(t, ctx, svc, seed.principalID, nil, 0, 0)
	if report.BusinessCount != 1 || report.ActiveAutomations != 0 || report.ActiveEnrollments != 0 {
		t.Fatalf("empty business = %+v", report)
	}
	reportingRate(t, "empty opens", report.OpenRate, 0, 0)
	reportingRate(t, "empty clicks", report.ClickRate, 0, 0)
	reportingRate(t, "empty unsubscribes", report.UnsubscribeRate, 0, 0)
	list := reportingList(t, ctx, svc, seed.principalID, seed.businessID, "Untracked")
	sub := reportingSubscriber(t, ctx, svc, seed.principalID, list, "untracked@example.test")
	reportingExec(t, ctx, database, `WITH template AS (
		INSERT INTO mailing_template(business_id,tenant_root_id,name,subject,body_markdown,track_opens,track_clicks)
		VALUES($1,$1,'Mutable template','Subject','Body',true,true) RETURNING id
	) INSERT INTO mailing_delivery(business_id,tenant_root_id,source_kind,source_id,template_id,subscriber_id,email,status,message_id,
		track_opens_override,track_clicks_override,opened_at,first_clicked_at)
	SELECT $1,$1,'automation',gen_random_uuid(),id,$2,$3,'sent',gen_random_uuid()::text,false,false,now(),now() FROM template`, seed.businessID, sub.ID, sub.Email)
	report = reportingBalance(t, ctx, svc, seed.principalID, nil, 1, 1)
	reportingRate(t, "disabled opens", report.OpenRate, 0, 0)
	reportingRate(t, "disabled clicks", report.ClickRate, 0, 0)
	reportingRate(t, "observed zero unsubscribe", report.UnsubscribeRate, 0, 1)
	// A role without the module permission sees a real empty scope, not mailing
	// data through membership-only RLS. Filtered access remains 404 above.
	reader := seedMailingWriteOnlyPrincipal(ctx, t, database, seed.businessID)
	reportingExec(t, ctx, database, `DELETE FROM role_permission WHERE permission_key='mailing.read'
		AND role_id IN (SELECT role_id FROM membership WHERE principal_id=$1)`, reader)
	report = reportingBalance(t, ctx, svc, reader, nil, 0, 0)
	if report.BusinessCount != 0 || report.TenantCount != 0 {
		t.Fatalf("no-permission scope = %+v", report)
	}
	reportingRate(t, "hidden accepted messages", report.UnsubscribeRate, 0, 0)
}

func TestMailingReportingMigrationBaselineLifecycleAndErasure(t *testing.T) {
	ctx, database, svc := reportingDB(t)
	seed := seedMailingTenant(ctx, t, database)
	first := reportingList(t, ctx, svc, seed.principalID, seed.businessID, "First")
	second := reportingList(t, ctx, svc, seed.principalID, seed.businessID, "Second")
	shared := reportingSubscriber(t, ctx, svc, seed.principalID, first, "same@example.test")
	_ = reportingSubscriber(t, ctx, svc, seed.principalID, second, "SAME@example.test")
	depart := reportingSubscriber(t, ctx, svc, seed.principalID, first, "depart@example.test")
	reportingExec(t, ctx, database, `UPDATE list_subscriber SET created_at=now()-interval '20 days',
		confirmed_at=now()-interval '20 days',updated_at=now()-interval '20 days' WHERE tenant_root_id=$1`, seed.businessID)
	for _, file := range []string{"0137_mailing_reporting.down.sql", "0137_mailing_reporting.up.sql"} {
		raw, err := migrations.FS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		reportingExec(t, ctx, database, string(raw))
	}
	baseline := reportingBalance(t, ctx, svc, seed.principalID, nil, 2, 0)
	if baseline.SubscriberWindowComplete || !baseline.SubscriberWindowStart.After(baseline.WindowStart) {
		t.Fatalf("migration fabricated a backfill: %+v", baseline)
	}
	// started_at == baseline must count: the initial net is zero, not +2.
	reportingExec(t, ctx, database, `UPDATE list_subscriber SET status='unsubscribed' WHERE id=$1`, shared.ID)
	reportingBalance(t, ctx, svc, seed.principalID, nil, 2, 0)
	if err := svc.ArchiveList(ctx, seed.principalID, seed.businessID, second.ID); err != nil {
		t.Fatal(err)
	}
	reportingBalance(t, ctx, svc, seed.principalID, nil, 1, -1)
	reportingExec(t, ctx, database, `UPDATE mailing_list SET status='active' WHERE id=$1`, second.ID)
	reportingBalance(t, ctx, svc, seed.principalID, nil, 2, 0)
	reportingExec(t, ctx, database, `DELETE FROM list_subscriber WHERE id=$1`, depart.ID)
	reportingBalance(t, ctx, svc, seed.principalID, nil, 1, -1)
	newcomer := reportingSubscriber(t, ctx, svc, seed.principalID, first, "new@example.test")
	reportingBalance(t, ctx, svc, seed.principalID, nil, 2, 0)
	// Net, not event count: a join followed by removal in the same window is zero.
	temporary := reportingSubscriber(t, ctx, svc, seed.principalID, first, "temporary@example.test")
	reportingExec(t, ctx, database, `DELETE FROM list_subscriber WHERE id=$1`, temporary.ID)
	reportingBalance(t, ctx, svc, seed.principalID, nil, 2, 0)
	tx, err := database.Super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE list_subscriber SET status='unsubscribed' WHERE id=$1`, newcomer.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	reportingBalance(t, ctx, svc, seed.principalID, nil, 2, 0)

	// An exact upper start boundary is exclusive. The departed membership's end
	// becomes this business's declared baseline; it must not remain in that start.
	reportingExec(t, ctx, database, `UPDATE mailing_reporting_business SET history_started_at=(
		SELECT ended_at FROM mailing_reporting_membership m JOIN mailing_reporting_state s ON true
		WHERE m.identity_fingerprint=hmac(convert_to('depart@example.test','UTF8'),s.identity_key,'sha256')
	) WHERE business_id=$1`, seed.businessID)
	reportingBalance(t, ctx, svc, seed.principalID, nil, 2, 1)
	if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT mailing_reporting_erase($1,$2)`, seed.businessID, "depart@example.test")
		return err
	}); err == nil {
		t.Fatal("application role could erase reporting history")
	}
	reportingExec(t, ctx, database, `SET ROLE manyforge_erasure; SELECT mailing_reporting_erase('`+seed.businessID.String()+`','depart@example.test'); RESET ROLE`)
	erased := reportingBalance(t, ctx, svc, seed.principalID, nil, 2, 0)
	if !erased.SubscriberWindowStart.After(baseline.SubscriberWindowStart) {
		t.Fatal("erasure did not declare the shortened trustworthy history")
	}
	var erasedRows int
	if err := database.Super.QueryRow(ctx, `SELECT count(*) FROM mailing_reporting_membership m JOIN mailing_reporting_state s ON true
		WHERE identity_fingerprint=hmac(convert_to('depart@example.test','UTF8'),s.identity_key,'sha256')`).Scan(&erasedRows); err != nil || erasedRows != 0 {
		t.Fatalf("erased pseudonym rows=%d err=%v", erasedRows, err)
	}
	// Deleting a list cascades its source memberships, but must preserve the
	// historical start: physical removal is not allowed to erase attrition.
	reportingExec(t, ctx, database, `DELETE FROM mailing_list WHERE id=$1`, second.ID)
	reportingBalance(t, ctx, svc, seed.principalID, nil, 1, -1)
}

func TestMailingReportingConcurrentListAndMembershipHistory(t *testing.T) {
	ctx, database, svc := reportingDB(t)
	seed := seedMailingTenant(ctx, t, database)
	list := reportingList(t, ctx, svc, seed.principalID, seed.businessID, "Concurrent")
	one := reportingSubscriber(t, ctx, svc, seed.principalID, list, "one@example.test")
	// Hold both source changes uncommitted. List-state and subscriber-state
	// history must not require each other's locks or count stale other-table rows.
	listTx, err := database.Super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listTx.Rollback(ctx)
	subscriberTx, err := database.Super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer subscriberTx.Rollback(ctx)
	if _, err := listTx.Exec(ctx, `UPDATE mailing_list SET status='archived' WHERE id=$1`, list.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := subscriberTx.Exec(ctx, `UPDATE list_subscriber SET status='unsubscribed' WHERE id=$1`, one.ID); err != nil {
		t.Fatal(err)
	}
	if err := subscriberTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := listTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	reportingBalance(t, ctx, svc, seed.principalID, nil, 0, 0)
	reportingExec(t, ctx, database, `UPDATE mailing_list SET status='active' WHERE id=$1`, list.ID)
	reportingExec(t, ctx, database, `UPDATE list_subscriber SET status='active' WHERE id=$1`, one.ID)
	reportingBalance(t, ctx, svc, seed.principalID, nil, 1, 1)
}

func TestMailingReportingRetentionAndMerge(t *testing.T) {
	ctx, database, svc := reportingDB(t)
	source := seedMailingTenant(ctx, t, database)
	destination := seedMailingTenant(ctx, t, database)
	reportingExec(t, ctx, database, `INSERT INTO membership(principal_id,business_id,tenant_root_id,role_id)
		SELECT $1,$2,$2,id FROM role WHERE tenant_root_id IS NULL AND key='owner'`, source.principalID, destination.businessID)
	for _, seed := range []mailingSeed{source, destination} {
		list := reportingList(t, ctx, svc, seed.principalID, seed.businessID, "Merge history")
		_ = reportingSubscriber(t, ctx, svc, seed.principalID, list, "same@example.test")
		depart := reportingSubscriber(t, ctx, svc, seed.principalID, list, "depart@example.test")
		reportingExec(t, ctx, database, `UPDATE list_subscriber SET status='unsubscribed' WHERE id=$1`, depart.ID)
	}
	// Explicit old fixture intervals exercise a complete seven-day start balance
	// without waiting a week or pretending source timestamps are lifecycle history.
	reportingExec(t, ctx, database, `UPDATE mailing_reporting_state SET history_started_at=now()-interval '10 days'`)
	reportingExec(t, ctx, database, `UPDATE mailing_reporting_business SET history_started_at=now()-interval '10 days'`)
	reportingExec(t, ctx, database, `UPDATE mailing_reporting_membership SET started_at=now()-interval '9 days'`)
	reportingExec(t, ctx, database, `UPDATE mailing_reporting_list SET started_at=now()-interval '9 days'`)
	before := reportingBalance(t, ctx, svc, source.principalID, nil, 2, -2)
	if !before.SubscriberWindowComplete || !before.SubscriberWindowStart.Equal(before.WindowStart) {
		t.Fatalf("complete history = %+v", before)
	}
	// Closed history crossing the start survives; already-closed older history
	// is pruned by the SAME maintenance worker production starts unconditionally.
	reportingExec(t, ctx, database, `INSERT INTO mailing_reporting_membership(business_id,tenant_root_id,list_id,identity_fingerprint,started_at,ended_at)
		SELECT business_id,tenant_root_id,list_id,identity_fingerprint,now()-interval '20 days',now()-interval '8 days'
		FROM mailing_reporting_membership WHERE ended_at IS NOT NULL`)
	if _, _, err := (&timeseries.MaintenanceWorker{DB: database.App}).SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	reportingBalance(t, ctx, svc, source.principalID, nil, 2, -2)
	var expired int
	if err := database.Super.QueryRow(ctx, `SELECT count(*) FROM mailing_reporting_membership WHERE ended_at < now()-interval '7 days'`).Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("expired intervals=%d err=%v", expired, err)
	}
	reportingExec(t, ctx, database, `INSERT INTO mailing_reporting_membership(business_id,tenant_root_id,list_id,identity_fingerprint,started_at,ended_at)
		SELECT business_id,tenant_root_id,list_id,identity_fingerprint,now()-interval '20 days',now()-interval '8 days'
		FROM mailing_reporting_membership WHERE ended_at IS NOT NULL`)

	// Remove processed fixture events so the real merge can drain; history is
	// reconciled by the ordinary inventory and never re-created during root moves.
	reportingExec(t, ctx, database, `UPDATE outbox SET processed_at=now() WHERE processed_at IS NULL`)
	merge := &tenancy.Service{DB: database.App}
	operation, err := merge.CreateTenantMergeOperation(ctx, source.principalID, source.businessID, destination.businessID, "reporting-merge")
	if err != nil {
		t.Fatal(err)
	}
	operation, err = merge.PreflightTenantMerge(ctx, source.principalID, operation.ID)
	if err != nil || operation.Status != "ready" {
		t.Fatalf("history merge preflight = %+v, err=%v", operation, err)
	}
	reportingExec(t, ctx, database, `UPDATE tenant_merge_operation SET confirmed_at=now(),
		confirmation_method='password_and_typed_names',confirmation_hash=repeat('a',64),
		confirmation_preflight_generation=preflight_generation WHERE id=$1`, operation.ID)
	operation, err = merge.BeginTenantMergeFence(ctx, source.principalID, operation.ID)
	if err != nil || operation.Status != "running" {
		t.Fatalf("fence history merge = %+v, err=%v", operation, err)
	}
	if err := database.App.WithTx(ctx, func(tx pgx.Tx) error {
		var removed int64
		return tx.QueryRow(ctx, `SELECT mailing_reporting_prune()`).Scan(&removed)
	}); err != nil {
		t.Fatalf("retention should skip fenced roots: %v", err)
	}
	if err := database.Super.QueryRow(ctx, `SELECT count(*) FROM mailing_reporting_membership WHERE ended_at < now()-interval '7 days'`).Scan(&expired); err != nil || expired != 2 {
		t.Fatalf("retention changed fenced history: expired=%d err=%v", expired, err)
	}
	operation, err = merge.CutoverTenantMerge(ctx, source.principalID, operation.ID)
	if err != nil || operation.Status != "succeeded" {
		t.Fatalf("history merge cutover = %+v, err=%v", operation, err)
	}
	after := reportingBalance(t, ctx, svc, source.principalID, nil, 1, -1)
	if after.BusinessCount != 2 || after.TenantCount != 1 {
		t.Fatalf("post-merge current scope = %+v", after)
	}
	for _, table := range []string{"mailing_reporting_business", "mailing_reporting_membership", "mailing_reporting_list"} {
		var residue int
		if err := database.Super.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE tenant_root_id=$1", table), source.businessID).Scan(&residue); err != nil || residue != 0 {
			t.Fatalf("%s source residue=%d err=%v", table, residue, err)
		}
	}
	_ = seedSibling(ctx, t, database, destination.businessID)
	expanded := reportingBalance(t, ctx, svc, source.principalID, nil, 1, -1)
	if !expanded.SubscriberWindowComplete || !expanded.SubscriberWindowStart.Equal(expanded.WindowStart) {
		t.Fatalf("a new empty business shortened existing history: %+v", expanded)
	}
}
