package notify

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestLogSenderLogsMetadataOnlyAndDoesNotAccept(t *testing.T) {
	var output bytes.Buffer
	sender := LogSender{Logger: slog.New(slog.NewTextHandler(&output, nil))}
	err := sender.Send(context.Background(), Mail{
		From: "news@example.test", To: "reader@example.test", Subject: "Private subject",
		BodyText: "https://manyforge.test/m/u/private-capability", BodyHTML: "<p>private body</p>",
		MessageID: "delivery-1@mail.example.test", ReplyTo: "reply+private@example.test",
	})
	if !errors.Is(err, ErrNotAccepted) {
		t.Fatalf("Send error = %v, want ErrNotAccepted", err)
	}
	logLine := output.String()
	if !strings.Contains(logLine, "message_id=delivery-1@mail.example.test") ||
		!strings.Contains(logLine, "has_text=true") || !strings.Contains(logLine, "has_html=true") {
		t.Fatalf("safe metadata missing from log: %s", logLine)
	}
	for _, secret := range []string{
		"news@example.test", "reader@example.test", "Private subject",
		"private-capability", "private body", "reply+private@example.test",
	} {
		if strings.Contains(logLine, secret) {
			t.Fatalf("log leaked %q: %s", secret, logLine)
		}
	}
}

func TestSMTPSenderWithoutHostDoesNotAccept(t *testing.T) {
	sender := NewSMTPSender(SMTPConfig{}, nil)
	if err := sender.Send(context.Background(), Mail{To: "reader@example.test"}); !errors.Is(err, ErrNotAccepted) {
		t.Fatalf("Send error = %v, want ErrNotAccepted", err)
	}
}

func TestDisabledSenderRejectsWithoutLogging(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var sender Sender = DisabledSender{}
	err := sender.Send(context.Background(), Mail{
		To: "reader@example.test", Subject: "Private subject",
		BodyText: "token: private-capability", BodyHTML: "<p>private body</p>",
	})
	if !errors.Is(err, ErrNotAccepted) {
		t.Fatalf("Send error = %v, want ErrNotAccepted", err)
	}
	if output.Len() != 0 {
		t.Fatalf("disabled sender logged message data: %s", output.String())
	}
}
