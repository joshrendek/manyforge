//go:build integration

package notify

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/migrations"
)

func TestSupportAdminNotifications(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "sent")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tdb.Super.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	var ticketID, owner uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT ticket_id, author_principal_id FROM ticket_message WHERE id=$1", st.messageRowID).Scan(&ticketID, &owner); err != nil {
		t.Fatal(err)
	}
	exec("UPDATE ticket_message SET direction='inbound', author_principal_id=NULL WHERE id=$1", st.messageRowID)
	exec("INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id) SELECT $1,$2,$2,id FROM role WHERE tenant_root_id IS NULL AND key='owner'", owner, st.master)
	addMember := func(businessID uuid.UUID, role string, verified bool) uuid.UUID {
		t.Helper()
		accountID, principalID := uuid.New(), uuid.New()
		exec("INSERT INTO account (id,email,display_name,email_verified_at) VALUES ($1,$2,'Member',CASE WHEN $3 THEN now() END)", accountID, accountID.String()+"@example.com", verified)
		exec("INSERT INTO principal (id,kind,account_id) VALUES ($1,'human',$2)", principalID, accountID)
		exec("INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id) SELECT $1,$2,$3,id FROM role WHERE tenant_root_id IS NULL AND key=$4", principalID, businessID, st.master, role)
		return principalID
	}
	admin := addMember(st.master, "admin", true)
	addMember(st.master, "member", true)
	addMember(st.master, "admin", false)
	// A child-only admin must not receive its parent's support traffic.
	child := uuid.New()
	exec("INSERT INTO business (id,parent_id,tenant_root_id,name) VALUES ($1,$2,$2,'Child')", child, st.master)
	exec("INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$2),($2,$1,1,$2)", child, st.master)
	addMember(child, "admin", true)
	cap := &captureSender{}
	sub := SupportAdminSubscriber{Sender: cap, From: "alerts@example.com", BaseURL: "https://app.example.com/", Suppression: DBSuppression{DB: tdb.App}}
	payload, _ := json.Marshal(map[string]any{"business_id": st.master, "ticket_id": ticketID, "message_id": st.messageRowID})
	inbound := events.Event{ID: uuid.New(), TenantRootID: st.master, Topic: events.TopicMessageReceived, Payload: payload}
	queue := func(e events.Event) {
		t.Helper()
		if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error { return sub.Queue(ctx, tx, e) }); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		var n int
		if err := tdb.Super.QueryRow(ctx, "SELECT count(*) FROM notification WHERE kind='support.inbound'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	queue(inbound)
	queue(inbound)
	if n := count(); n != 2 {
		t.Fatalf("notifications=%d, want owner and verified admin exactly once", n)
	}
	wrong := inbound
	wrong.TenantRootID = uuid.New()
	queue(wrong)
	wrong = inbound
	wrong.Payload = []byte(`{"business_id":"` + child.String() + `","ticket_id":"` + ticketID.String() + `","message_id":"` + st.messageRowID.String() + `"}`)
	queue(wrong)
	if n := count(); n != 2 {
		t.Fatalf("forged scope created notifications: %d", n)
	}
	rows, err := tdb.Super.Query(ctx, "SELECT id,tenant_root_id,topic,payload FROM outbox WHERE topic=$1 ORDER BY id", events.TopicSupportAdminNotify)
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (events.Event, error) {
		var e events.Event
		err := row.Scan(&e.ID, &e.TenantRootID, &e.Topic, &e.Payload)
		return e, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("deliveries=%d", len(deliveries))
	}
	for i := range deliveries {
		deliveries[i].MaxAttempts = 10
	}
	send := func(e events.Event) error {
		return tdb.App.WithTx(ctx, func(tx pgx.Tx) error { return sub.Send(ctx, tx, e) })
	}
	sub.Suppression = failingSuppression{}
	if err := send(deliveries[0]); err == nil {
		t.Fatal("suppression lookup failure did not retry")
	}
	if cap.calls != 0 {
		t.Fatal("suppression lookup failure dispatched email")
	}
	sub.Suppression = DBSuppression{DB: tdb.App}
	cap.err = errors.New("secret upstream content")
	if err := send(deliveries[0]); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("retry error=%v", err)
	}
	firstID := cap.last.MessageID
	cap.err = nil
	if err := send(deliveries[0]); err != nil {
		t.Fatal(err)
	}
	if cap.last.MessageID != firstID {
		t.Fatal("retry changed provider idempotency key")
	}
	if !strings.Contains(cap.last.BodyText, "https://app.example.com/support/"+st.master.String()+"/"+ticketID.String()) {
		t.Fatal("missing ticket deep link")
	}
	if cap.last.AutoSubmitted != "auto-generated" || cap.last.ReplyTo != "" {
		t.Fatal("notification must not become a customer reply")
	}
	raw, err := BuildMIME(*cap.last)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "Auto-Submitted:") != 1 {
		t.Fatal("automatic mail header must appear exactly once")
	}
	calls := cap.calls
	if err := send(deliveries[0]); err != nil {
		t.Fatal(err)
	}
	if cap.calls != calls {
		t.Fatal("redelivery resent accepted notification")
	}
	wrong = deliveries[1]
	wrong.TenantRootID = uuid.New()
	if err := send(wrong); err != nil {
		t.Fatal(err)
	}
	if cap.calls != calls {
		t.Fatal("foreign tenant dispatched email")
	}
	cap.err = ErrSuppressed
	if err := send(deliveries[1]); err != nil {
		t.Fatal(err)
	}
	if err := send(deliveries[1]); err != nil {
		t.Fatal(err)
	}
	if cap.calls != calls+1 {
		t.Fatal("suppression redelivery retried")
	}
	// A new inbound reply generates another pair of notifications on the same ticket.
	replyID := uuid.New()
	exec("INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,message_id,body_text,delivery_state) VALUES ($1,$2,$3,$3,'inbound',$4,'Customer followup','sent')", replyID, ticketID, st.master, replyID.String()+"@example.com")
	inbound.Payload, _ = json.Marshal(map[string]any{"business_id": st.master, "ticket_id": ticketID, "message_id": replyID})
	queue(inbound)
	if n := count(); n != 4 {
		t.Fatalf("reply notifications=%d, want 4", n)
	}
	// Access is checked again before delivery, so a revoked admin receives nothing.
	var pendingID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM notification WHERE principal_id=$1 AND ref->>'message_id'=$2", admin, replyID.String()).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	exec("DELETE FROM membership WHERE principal_id=$1", admin)
	cap.err = nil
	calls = cap.calls
	data, _ := json.Marshal(map[string]any{"notification_id": pendingID})
	if err := send(events.Event{TenantRootID: st.master, Payload: data}); err != nil {
		t.Fatal(err)
	}
	if cap.calls != calls {
		t.Fatal("revoked admin received email")
	}
	// Auto replies and outbound/internal messages never fan out admin alerts.
	exec("UPDATE ticket_message SET is_auto_reply=true WHERE id=$1", replyID)
	queue(inbound)
	if n := count(); n != 4 {
		t.Fatalf("auto-reply notified: %d", n)
	}
	// Existing business-owned support profiles are used even with no shared SMTP.
	_, profileID := seedResendIdentity(ctx, t, tdb, st, "verified")
	exec("UPDATE ticket_message SET is_auto_reply=false WHERE id=$1", replyID)
	profile := &captureProfileSender{}
	sub.Sender = nil
	sub.From = ""
	sub.ProfileSender = profile
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM notification WHERE principal_id=$1 AND ref->>'message_id'=$2", owner, replyID.String()).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(map[string]any{"notification_id": pendingID})
	if err := send(events.Event{TenantRootID: st.master, Payload: data}); err != nil {
		t.Fatal(err)
	}
	if profile.calls != 1 || profile.profileID != profileID {
		t.Fatal("business support profile not used")
	}
	// Inherited and direct grants for the same principal produce a single alert.
	exec("INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id) SELECT $1,$2,$3,id FROM role WHERE tenant_root_id IS NULL AND key='admin'", owner, child, st.master)
	childTicket, childMessage := uuid.New(), uuid.New()
	exec("INSERT INTO ticket (id,business_id,tenant_root_id,requester_id,subject,reply_token) SELECT $1,$2,tenant_root_id,requester_id,'Child request',$3 FROM ticket WHERE id=$4", childTicket, child, childTicket.String(), ticketID)
	exec("INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,message_id,body_text) VALUES ($1,$2,$3,$4,'inbound',$5,'Child message')", childMessage, childTicket, child, st.master, childMessage.String()+"@example.com")
	inbound.Payload, _ = json.Marshal(map[string]any{"business_id": child, "ticket_id": childTicket, "message_id": childMessage})
	queue(inbound)
	queue(inbound)
	if n := count(); n != 6 {
		t.Fatalf("inherited/direct fanout=%d, want 6", n)
	}
	// Fresh auto-generated and staff messages must be rejected before fanout.
	autoID := uuid.New()
	exec("INSERT INTO ticket_message (id,ticket_id,business_id,tenant_root_id,direction,message_id,body_text,is_auto_reply) VALUES ($1,$2,$3,$3,'inbound',$4,'Auto reply',true)", autoID, ticketID, st.master, autoID.String()+"@example.com")
	inbound.Payload, _ = json.Marshal(map[string]any{"business_id": st.master, "ticket_id": ticketID, "message_id": autoID})
	queue(inbound)
	exec("UPDATE ticket_message SET is_auto_reply=false,direction='note',author_principal_id=$2 WHERE id=$1", autoID, owner)
	queue(inbound)
	if n := count(); n != 6 {
		t.Fatalf("auto reply or staff note fanned out: %d", n)
	}
	exec("UPDATE ticket_message SET direction='inbound',author_principal_id=NULL WHERE id=$1", autoID)
	exec("UPDATE ticket SET redacted_at=now() WHERE id=$1", ticketID)
	queue(inbound)
	if n := count(); n != 6 {
		t.Fatal("redacted ticket queued admin email")
	}
	exec("UPDATE ticket SET redacted_at=NULL WHERE id=$1", ticketID)
	queue(inbound)
	if n := count(); n != 7 {
		t.Fatalf("fresh customer message did not fan out: %d", n)
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM notification WHERE ref->>'message_id'=$1", autoID.String()).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(map[string]any{"notification_id": pendingID})
	exec("UPDATE ticket SET redacted_at=now() WHERE id=$1", ticketID)
	profileCalls := profile.calls
	if err := send(events.Event{TenantRootID: st.master, Payload: data, MaxAttempts: 10}); err != nil {
		t.Fatal(err)
	}
	if profile.calls != profileCalls {
		t.Fatal("ticket redacted after queue dispatched email")
	}
	exec("UPDATE ticket SET redacted_at=NULL WHERE id=$1", ticketID)
	profile.err = errors.New("private provider response")
	if err := send(events.Event{TenantRootID: st.master, Payload: data, Attempts: 9, MaxAttempts: 10}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := tdb.Super.QueryRow(ctx, "SELECT ref->>'email_state' FROM notification WHERE id=$1", pendingID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("terminal state=%s", state)
	}
	// The production database suppression checker short-circuits the transport.
	if err := tdb.Super.QueryRow(ctx, "SELECT id FROM notification WHERE principal_id=$1 AND ref->>'message_id'=$2", owner, childMessage.String()).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO email_suppression(email,reason) SELECT a.email,'hard_bounce' FROM account a JOIN principal p ON p.account_id=a.id WHERE p.id=$1", owner)
	sub.Sender = cap
	sub.From = "alerts@example.com"
	data, _ = json.Marshal(map[string]any{"notification_id": pendingID})
	calls = cap.calls
	if err := send(events.Event{TenantRootID: st.master, Payload: data, MaxAttempts: 10}); err != nil {
		t.Fatal(err)
	}
	if cap.calls != calls {
		t.Fatal("database-suppressed recipient reached transport")
	}
	if err := tdb.Super.QueryRow(ctx, "SELECT ref->>'email_state' FROM notification WHERE id=$1", pendingID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "suppressed" {
		t.Fatalf("database suppression state=%s", state)
	}
	// Migration rollback and reapply must preserve the existing merge contract.
	for _, filename := range []string{"0150_support_admin_notifications.down.sql", "0150_support_admin_notifications.up.sql"} {
		sql, err := migrations.FS.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		exec(string(sql))
	}
	var registered bool
	if err := tdb.Super.QueryRow(ctx, "SELECT position('support.admin.notify' IN pg_get_functiondef('tenant_merge_preflight_inventory_v1(uuid,uuid)'::regprocedure)) > 0").Scan(&registered); err != nil {
		t.Fatal(err)
	}
	if !registered {
		t.Fatal("notification topic missing from tenant merge contract")
	}
}
