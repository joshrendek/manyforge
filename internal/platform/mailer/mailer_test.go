package mailer

import (
	"context"
	"errors"
	"testing"
)

type spyMailer struct{ sent int }

func (s *spyMailer) Send(context.Context, Message) error { s.sent++; return nil }

type staticChecker struct {
	suppressed bool
	err        error
}

func (c staticChecker) IsSuppressed(context.Context, string) (bool, error) {
	return c.suppressed, c.err
}

func TestLogMailerSend(t *testing.T) {
	if err := (LogMailer{}).Send(context.Background(), Message{To: "a@b.test", Subject: "hi"}); err != nil {
		t.Fatalf("log mailer send: %v", err)
	}
}

func TestGuardedSkipsSuppressed(t *testing.T) {
	spy := &spyMailer{}
	g := Guarded{Mailer: spy, Checker: staticChecker{suppressed: true}}
	if err := g.Send(context.Background(), Message{To: "bounced@b.test"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if spy.sent != 0 {
		t.Errorf("suppressed address should not be sent (sent=%d)", spy.sent)
	}
}

func TestGuardedSendsAllowed(t *testing.T) {
	spy := &spyMailer{}
	g := Guarded{Mailer: spy, Checker: staticChecker{suppressed: false}}
	if err := g.Send(context.Background(), Message{To: "ok@b.test"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if spy.sent != 1 {
		t.Errorf("allowed address should be sent once (sent=%d)", spy.sent)
	}
}

func TestGuardedPropagatesCheckerError(t *testing.T) {
	spy := &spyMailer{}
	g := Guarded{Mailer: spy, Checker: staticChecker{err: errors.New("db down")}}
	if err := g.Send(context.Background(), Message{To: "x@b.test"}); err == nil {
		t.Error("checker error should propagate")
	}
	if spy.sent != 0 {
		t.Errorf("must not send when checker errors (sent=%d)", spy.sent)
	}
}
