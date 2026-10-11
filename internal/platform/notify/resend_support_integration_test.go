//go:build integration

package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
)

type captureProfileSender struct {
	calls     int
	profileID uuid.UUID
	last      *Mail
	err       error
}

func (c *captureProfileSender) SendSupportReply(_ context.Context, profileID uuid.UUID, m Mail) error {
	c.calls++
	c.profileID = profileID
	c.last = &m
	return c.err
}

type staticSuppression struct{ suppressed bool }

func (s staticSuppression) IsSuppressed(context.Context, string) (bool, error) {
	return s.suppressed, nil
}

// seedResendIdentity adds a verified provider_route domain, a custom support@
// address, and a resend-mode profile in the given status to a seeded sendTenant.
func seedResendIdentity(ctx context.Context, t *testing.T, tdb *testdb.TestDB, st sendTenant, profileStatus string) (customAddr string, profileID uuid.UUID) {
	t.Helper()
	domain := "support-" + st.master.String()[:8] + ".example.com"
	customAddr = "support@" + domain
	domainID, secretID := uuid.New(), uuid.New()
	profileID = uuid.New()
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO email_domain (id,business_id,tenant_root_id,domain,mode,verify_token,verified_at,spf_state,created_at,updated_at)
		  VALUES ($1,$2,$2,$3,'provider_route','mf-verify=seed',now(),'unknown',now(),now())`, []any{domainID, st.master, domain}},
		{`INSERT INTO inbound_address (id,business_id,tenant_root_id,address,kind,email_domain_id,created_at,updated_at)
		  VALUES ($1,$2,$2,$3,'custom',$4,now(),now())`, []any{uuid.New(), st.master, customAddr, domainID}},
		{`INSERT INTO secret (id,business_id,tenant_root_id,scope,sealed_value,created_at,updated_at)
		  VALUES ($1,$2,$2,'mailing','sealed',now(),now())`, []any{secretID, st.master}},
		{`INSERT INTO mailing_sending_profile (id,business_id,tenant_root_id,mode,from_email,from_name,secret_ref,status,feedback_status,feedback_confirmed_at)
		  VALUES ($1,$2,$2,'resend',$3,'Support',$4,$5,'ready',now())`, []any{profileID, st.master, customAddr, secretID, profileStatus}},
	}
	for _, s := range stmts {
		if _, err := tdb.Super.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed resend identity: %v\nSQL: %s", err, s.sql)
		}
	}
	return customAddr, profileID
}

func handleWith(ctx context.Context, t *testing.T, tdb *testdb.TestDB, sub SendSubscriber, ev events.Event) error {
	t.Helper()
	return tdb.App.WithTx(ctx, func(tx pgx.Tx) error { return sub.Handle(ctx, tx, ev) })
}

// R10: From is the custom address, Reply-To its plus-addressed token form, and the
// shared transport is never touched.
func TestSendViaResendSupportProfile(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	customAddr, profileID := seedResendIdentity(ctx, t, tdb, st, "verified")
	shared := &captureSender{}
	profile := &captureProfileSender{}
	sub := SendSubscriber{Sender: shared, ProfileSender: profile, Suppression: staticSuppression{}}

	if err := handleWith(ctx, t, tdb, sub, repliedEvent(t, st)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if shared.calls != 0 {
		t.Fatalf("shared transport used %d times, want 0", shared.calls)
	}
	if profile.calls != 1 || profile.profileID != profileID {
		t.Fatalf("profile sender calls=%d profile=%v, want 1/%v", profile.calls, profile.profileID, profileID)
	}
	wantReplyTo := "support+" + st.replyToken + "@" + customAddr[len("support@"):]
	if profile.last.From != customAddr || profile.last.ReplyTo != wantReplyTo || profile.last.DKIM != nil {
		t.Fatalf("mail From=%q ReplyTo=%q DKIM=%v, want %q / %q / nil", profile.last.From, profile.last.ReplyTo, profile.last.DKIM, customAddr, wantReplyTo)
	}
	if profile.last.MessageID != st.rfcMessageID || profile.last.InReplyTo != "parent-1@example.com" || profile.last.BodyText != st.replyBody {
		t.Fatalf("threading/body not carried: %+v", profile.last)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "sent" {
		t.Fatalf("delivery_state = %q, want sent", got)
	}
}

// R11 + R13: a failing profile retries (pending) and, on the final attempt, marks
// failed — never falling back to the shared transport.
func TestSendResendProfileFailureRetriesThenFails(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	seedResendIdentity(ctx, t, tdb, st, "verified")
	shared := &captureSender{}
	profile := &captureProfileSender{err: errors.New("provider: http 403 validation_error: ")}
	sub := SendSubscriber{Sender: shared, ProfileSender: profile}

	ev := repliedEvent(t, st)
	ev.Attempts, ev.MaxAttempts = 0, 3
	if err := handleWith(ctx, t, tdb, sub, ev); err == nil {
		t.Fatal("non-final failure returned nil, want an error so the worker retries")
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "pending" {
		t.Fatalf("after non-final failure delivery_state = %q, want pending", got)
	}
	ev.Attempts = 2
	if err := handleWith(ctx, t, tdb, sub, ev); err != nil {
		t.Fatalf("final failure returned %v, want nil (terminal outcome recorded)", err)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "failed" {
		t.Fatalf("after final failure delivery_state = %q, want failed", got)
	}
	if shared.calls != 0 {
		t.Fatalf("shared transport used %d times, want 0 (no fallback)", shared.calls)
	}
}

// R11: an unverified profile does not fall back either.
func TestSendResendProfileUnverifiedNoFallback(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	seedResendIdentity(ctx, t, tdb, st, "error")
	shared := &captureSender{}
	profile := &captureProfileSender{}
	sub := SendSubscriber{Sender: shared, ProfileSender: profile}
	ev := repliedEvent(t, st)
	ev.Attempts, ev.MaxAttempts = 9, 10
	if err := handleWith(ctx, t, tdb, sub, ev); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if shared.calls != 0 || profile.calls != 0 {
		t.Fatalf("shared=%d profile=%d, want 0/0 for an unverified profile", shared.calls, profile.calls)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "failed" {
		t.Fatalf("delivery_state = %q, want failed", got)
	}
}

func TestSendResendSuppressedRecipient(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	seedResendIdentity(ctx, t, tdb, st, "verified")
	profile := &captureProfileSender{}
	sub := SendSubscriber{Sender: &captureSender{}, ProfileSender: profile, Suppression: staticSuppression{suppressed: true}}
	if err := handleWith(ctx, t, tdb, sub, repliedEvent(t, st)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if profile.calls != 0 || deliveryState(ctx, t, tdb, st.messageRowID) != "failed" {
		t.Fatalf("suppressed recipient: profile calls=%d state=%q, want 0/failed", profile.calls, deliveryState(ctx, t, tdb, st.messageRowID))
	}
}

// R12 + R13 on the shared path: no Resend identity keeps the shared transport,
// and its final failed attempt is marked failed instead of dead-lettering pending.
func TestSendSharedTransportFinalAttemptMarksFailed(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	shared := &captureSender{err: errors.New("relay down")}
	profile := &captureProfileSender{}
	sub := SendSubscriber{Sender: shared, ProfileSender: profile}
	ev := repliedEvent(t, st)
	ev.Attempts, ev.MaxAttempts = 9, 10
	if err := handleWith(ctx, t, tdb, sub, ev); err != nil {
		t.Fatalf("final shared failure returned %v, want nil", err)
	}
	if shared.calls != 1 || profile.calls != 0 {
		t.Fatalf("shared=%d profile=%d, want 1/0", shared.calls, profile.calls)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "failed" {
		t.Fatalf("delivery_state = %q, want failed", got)
	}
}

type failingSuppression struct{}

func (failingSuppression) IsSuppressed(context.Context, string) (bool, error) {
	return false, errors.New("suppression store down")
}

// R13: a suppression lookup that keeps failing still ends the reply as failed on
// the final attempt instead of leaving it pending, and never sends.
func TestSendResendSuppressionErrorFinalAttemptMarksFailed(t *testing.T) {
	ctx, tdb := startSendDB(t)
	st := seedSendTenant(ctx, t, tdb, "pending")
	seedResendIdentity(ctx, t, tdb, st, "verified")
	profile := &captureProfileSender{}
	sub := SendSubscriber{Sender: &captureSender{}, ProfileSender: profile, Suppression: failingSuppression{}}
	ev := repliedEvent(t, st)
	ev.Attempts, ev.MaxAttempts = 0, 3
	if err := handleWith(ctx, t, tdb, sub, ev); err == nil {
		t.Fatal("non-final suppression error returned nil, want an error so the worker retries")
	}
	ev.Attempts = 2
	if err := handleWith(ctx, t, tdb, sub, ev); err != nil {
		t.Fatalf("final attempt returned %v, want nil", err)
	}
	if profile.calls != 0 {
		t.Fatalf("profile sender calls=%d, want 0", profile.calls)
	}
	if got := deliveryState(ctx, t, tdb, st.messageRowID); got != "failed" {
		t.Fatalf("delivery_state = %q, want failed", got)
	}
}
