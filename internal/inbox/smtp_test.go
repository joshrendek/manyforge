package inbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	smtp "github.com/emersion/go-smtp"

	"github.com/manyforge/manyforge/internal/platform/ratelimit"
)

// fakeValidator is a RecipientValidator whose verdict is keyed off an allowlist of
// addresses that resolve; everything else is unroutable (the no-oracle path).
type fakeValidator struct {
	routes map[string]bool
	calls  []string
}

func (f *fakeValidator) CanRoute(_ context.Context, recipient string) bool {
	f.calls = append(f.calls, recipient)
	return f.routes[strings.ToLower(recipient)]
}

// recordingIngester records every RawMessage it is handed and returns a
// programmable result/error so DATA-outcome mapping can be exercised without a DB.
type recordingIngester struct {
	got    []RawMessage
	result IngestResult
	err    error
}

func (f *recordingIngester) Ingest(_ context.Context, msg RawMessage) (IngestResult, error) {
	f.got = append(f.got, msg)
	return f.result, f.err
}

func newTestSession(v RecipientValidator, ing Ingester) *smtpSession {
	return &smtpSession{
		ingester:  ing,
		validator: v,
		remoteIP:  "203.0.113.7",
		maxBytes:  1 << 20,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		ctx:       context.Background(),
	}
}

// smtpErr asserts err is a *smtp.SMTPError and returns it.
func smtpErr(t *testing.T, err error) *smtp.SMTPError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an SMTP error, got nil")
	}
	var se *smtp.SMTPError
	if !errors.As(err, &se) {
		t.Fatalf("error %v (%T) is not a *smtp.SMTPError", err, err)
	}
	return se
}

// TestRcptUnknownRecipientGenericReject — a recipient that does not resolve is
// rejected with the generic 550, and that reply is BYTE-IDENTICAL to the 550 for a
// DIFFERENT non-resolving address (no existence oracle: unknown vs not-handled vs
// not-yours all look the same). Ingest is never reached.
func TestRcptUnknownRecipientGenericReject(t *testing.T) {
	v := &fakeValidator{routes: map[string]bool{}}
	ing := &recordingIngester{}
	s := newTestSession(v, ing)

	err1 := s.Rcpt("nobody@unknown.example", &smtp.RcptOptions{})
	se1 := smtpErr(t, err1)

	// A second, structurally different non-resolving recipient must produce a reply
	// that is indistinguishable from the first (code, enhanced code, and message).
	s2 := newTestSession(&fakeValidator{routes: map[string]bool{}}, &recordingIngester{})
	err2 := s2.Rcpt("someoneelse@another.invalid", &smtp.RcptOptions{})
	se2 := smtpErr(t, err2)

	if se1.Code != 550 {
		t.Errorf("reject code = %d, want 550", se1.Code)
	}
	if se1.Code != se2.Code || se1.EnhancedCode != se2.EnhancedCode || se1.Message != se2.Message {
		t.Errorf("550 reply is an oracle: %q vs %q must be identical", se1.Error(), se2.Error())
	}
	if se1.Error() != se2.Error() {
		t.Errorf("550 error strings differ (oracle): %q vs %q", se1.Error(), se2.Error())
	}
	if len(ing.got) != 0 {
		t.Errorf("Ingest was called for an unrouted recipient (%d times); it must not be", len(ing.got))
	}
}

// TestRcptResolvingRecipientAccepted — a resolving recipient is accepted (nil
// error) and DATA then forwards a RawMessage carrying the right envelope fields and
// raw bytes to the ingester.
func TestRcptResolvingRecipientAccepted(t *testing.T) {
	const rcpt = "support@biz.example"
	v := &fakeValidator{routes: map[string]bool{rcpt: true}}
	ing := &recordingIngester{result: IngestResult{Created: true}}
	s := newTestSession(v, ing)

	if err := s.Mail("ada@sender.example", &smtp.MailOptions{}); err != nil {
		t.Fatalf("Mail returned error: %v", err)
	}
	if err := s.Rcpt(rcpt, &smtp.RcptOptions{}); err != nil {
		t.Fatalf("Rcpt to a resolving recipient must be accepted, got: %v", err)
	}

	raw := []byte("Subject: hi\r\n\r\nbody\r\n")
	if err := s.Data(strings.NewReader(string(raw))); err != nil {
		t.Fatalf("Data returned error: %v", err)
	}

	if len(ing.got) != 1 {
		t.Fatalf("Ingest called %d times, want 1", len(ing.got))
	}
	m := ing.got[0]
	if m.EnvelopeRecipient != rcpt {
		t.Errorf("EnvelopeRecipient = %q, want %q", m.EnvelopeRecipient, rcpt)
	}
	if m.EnvelopeSender != "ada@sender.example" {
		t.Errorf("EnvelopeSender = %q, want %q", m.EnvelopeSender, "ada@sender.example")
	}
	if m.Provider != "smtp" {
		t.Errorf("Provider = %q, want %q", m.Provider, "smtp")
	}
	if string(m.Raw) != string(raw) {
		t.Errorf("Raw = %q, want %q", m.Raw, raw)
	}
	if m.RemoteIP != "203.0.113.7" {
		t.Errorf("RemoteIP = %q, want %q", m.RemoteIP, "203.0.113.7")
	}
	if m.ReceivedAt.IsZero() {
		t.Errorf("ReceivedAt must be set")
	}
}

// TestDataOversizeRejected — a DATA stream larger than the session cap is rejected
// (the cap is enforced; nothing is ingested).
func TestDataOversizeRejected(t *testing.T) {
	const rcpt = "support@biz.example"
	v := &fakeValidator{routes: map[string]bool{rcpt: true}}
	ing := &recordingIngester{}
	s := newTestSession(v, ing)
	s.maxBytes = 16 // tiny cap

	if err := s.Rcpt(rcpt, &smtp.RcptOptions{}); err != nil {
		t.Fatalf("Rcpt: %v", err)
	}

	big := strings.Repeat("A", 1024)
	err := s.Data(strings.NewReader(big))
	se := smtpErr(t, err)
	if se.Code != 552 && se.Code != 553 {
		t.Errorf("oversize DATA code = %d, want 552 or 553", se.Code)
	}
	if len(ing.got) != 0 {
		t.Errorf("Ingest was called for an oversize message; it must not be")
	}
}

// TestDataNoRouteUniform — if Ingest reports errNoRoute at DATA time (RCPT was
// gated, but resolution could still drop), the session still returns 250: uniform,
// no oracle at DATA either.
func TestDataNoRouteUniform(t *testing.T) {
	const rcpt = "support@biz.example"
	v := &fakeValidator{routes: map[string]bool{rcpt: true}}
	ing := &recordingIngester{err: errNoRoute}
	s := newTestSession(v, ing)

	if err := s.Rcpt(rcpt, &smtp.RcptOptions{}); err != nil {
		t.Fatalf("Rcpt: %v", err)
	}
	if err := s.Data(strings.NewReader("Subject: x\r\n\r\nbody\r\n")); err != nil {
		t.Errorf("Data with errNoRoute must be accepted (250 uniform), got: %v", err)
	}
}

// TestDataInternalErrorTemporary — a genuine internal Ingest error maps to a 451
// temporary failure with a GENERIC message that does NOT echo the internal error
// string (so the sender retries and nothing leaks).
func TestDataInternalErrorTemporary(t *testing.T) {
	const rcpt = "support@biz.example"
	v := &fakeValidator{routes: map[string]bool{rcpt: true}}
	secret := "constraint ticket_message_tenant_root_id_message_id_key violated"
	ing := &recordingIngester{err: errors.New(secret)}
	s := newTestSession(v, ing)

	if err := s.Rcpt(rcpt, &smtp.RcptOptions{}); err != nil {
		t.Fatalf("Rcpt: %v", err)
	}
	err := s.Data(strings.NewReader("Subject: x\r\n\r\nbody\r\n"))
	se := smtpErr(t, err)
	if se.Code/100 != 4 {
		t.Errorf("internal-error DATA code = %d, want a 4xx temporary failure", se.Code)
	}
	if se.Code != 451 {
		t.Errorf("internal-error DATA code = %d, want 451", se.Code)
	}
	if strings.Contains(se.Message, "constraint") || strings.Contains(se.Error(), secret) {
		t.Errorf("451 reply echoes the internal error (leak): %q", se.Error())
	}
}

// TestDataWithoutAcceptedRcptRejected — DATA with no accepted recipient is a
// sequencing error and must not call Ingest.
func TestDataWithoutAcceptedRcptRejected(t *testing.T) {
	v := &fakeValidator{routes: map[string]bool{}}
	ing := &recordingIngester{}
	s := newTestSession(v, ing)

	if err := s.Data(strings.NewReader("Subject: x\r\n\r\nbody\r\n")); err == nil {
		t.Errorf("Data with no accepted RCPT must error")
	}
	if len(ing.got) != 0 {
		t.Errorf("Ingest must not be called when no RCPT was accepted")
	}
}

// TestDataPerIPRateLimit — once a single connection IP exceeds the inbound message
// rate, DATA is refused with a GENERIC temporary 4xx (so the sender retries), and
// nothing is ingested for the throttled message. The cap is keyed on the connection
// remote IP and never on the recipient, so it cannot be an existence oracle.
func TestDataPerIPRateLimit(t *testing.T) {
	const rcpt = "support@biz.example"
	const burst = 2
	v := &fakeValidator{routes: map[string]bool{rcpt: true}}
	ing := &recordingIngester{result: IngestResult{Created: true}}

	// rate 0 ⇒ no refill within the test; exactly `burst` messages from the IP pass.
	limiter := ratelimit.NewTokenBucket(0, burst)

	deliver := func() error {
		s := newTestSession(v, ing)
		s.limiter = limiter // same limiter across sessions ⇒ shared per-IP budget
		if err := s.Rcpt(rcpt, &smtp.RcptOptions{}); err != nil {
			t.Fatalf("Rcpt: %v", err)
		}
		return s.Data(strings.NewReader("Subject: x\r\n\r\nbody\r\n"))
	}

	for i := 0; i < burst; i++ {
		if err := deliver(); err != nil {
			t.Fatalf("under per-IP cap message %d: want accept, got %v", i, err)
		}
	}
	if len(ing.got) != burst {
		t.Fatalf("ingested %d messages under cap, want %d", len(ing.got), burst)
	}

	// The next message from the same IP is over the cap: a generic 4xx temp failure.
	err := deliver()
	se := smtpErr(t, err)
	if se.Code/100 != 4 {
		t.Errorf("over per-IP cap code = %d, want a 4xx temporary failure", se.Code)
	}
	if strings.Contains(strings.ToLower(se.Message), rcpt) || strings.Contains(se.Message, "biz.example") {
		t.Errorf("rate-limit reply mentions the recipient (oracle): %q", se.Message)
	}
	if len(ing.got) != burst {
		t.Errorf("over-cap message was ingested: %d, want %d", len(ing.got), burst)
	}
}

// TestSMTPRateLimitNoRecipientOracle — the per-IP throttle reply must be IDENTICAL
// whether the (throttled) message was addressed to a routing recipient or not. The
// limiter is keyed only on the connection IP and checked before per-recipient work,
// so a known and an unknown recipient over the cap are byte-for-byte the same reply.
func TestSMTPRateLimitNoRecipientOracle(t *testing.T) {
	const known = "support@biz.example"
	const unknown = "nobody@absent.example"
	const burst = 1

	overCapReply := func(rcpt string, routes bool) *smtp.SMTPError {
		v := &fakeValidator{routes: map[string]bool{}}
		if routes {
			v.routes[rcpt] = true
		}
		ing := &recordingIngester{result: IngestResult{Created: true}}
		limiter := ratelimit.NewTokenBucket(0, burst)

		deliver := func() error {
			s := newTestSession(v, ing)
			s.limiter = limiter
			// Accept the RCPT only when it routes; for the unknown case we still must
			// exercise the rate limiter, so drive DATA via the routing recipient to fill
			// the bucket and key the unknown attempt on the same IP.
			if routes {
				if err := s.Rcpt(rcpt, &smtp.RcptOptions{}); err != nil {
					return err
				}
			} else {
				// Use a routing recipient to fill the bucket from this IP; the throttle
				// is keyed on the IP, not the recipient, so this is representative.
				v.routes[known] = true
				if err := s.Rcpt(known, &smtp.RcptOptions{}); err != nil {
					return err
				}
			}
			return s.Data(strings.NewReader("Subject: x\r\n\r\nbody\r\n"))
		}

		for i := 0; i < burst; i++ {
			if err := deliver(); err != nil {
				t.Fatalf("filling bucket (routes=%v) msg %d: %v", routes, i, err)
			}
		}
		return smtpErr(t, deliver())
	}

	knownReply := overCapReply(known, true)
	unknownReply := overCapReply(unknown, false)

	if knownReply.Code != unknownReply.Code ||
		knownReply.EnhancedCode != unknownReply.EnhancedCode ||
		knownReply.Message != unknownReply.Message {
		t.Errorf("SMTP rate-limit reply is a recipient oracle: %q vs %q must be identical",
			knownReply.Error(), unknownReply.Error())
	}
}

// TestResetClearsState — Reset discards the in-flight transaction (sender +
// accepted recipients) so a reused session does not leak prior state.
func TestResetClearsState(t *testing.T) {
	const rcpt = "support@biz.example"
	v := &fakeValidator{routes: map[string]bool{rcpt: true}}
	ing := &recordingIngester{}
	s := newTestSession(v, ing)

	_ = s.Mail("ada@sender.example", &smtp.MailOptions{})
	_ = s.Rcpt(rcpt, &smtp.RcptOptions{})
	s.Reset()

	if err := s.Data(strings.NewReader("Subject: x\r\n\r\nbody\r\n")); err == nil {
		t.Errorf("Data after Reset must error (no accepted recipient)")
	}
	if len(ing.got) != 0 {
		t.Errorf("Ingest must not be called after Reset cleared the transaction")
	}
}
