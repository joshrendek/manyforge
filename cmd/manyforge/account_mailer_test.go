package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/platform/mailer"
)

func TestAccountMailWithoutProductionTransportRejectsWithoutLoggingTokens(t *testing.T) {
	for _, cfg := range []config.Config{
		{Environment: "production"},
		{Environment: "development", OutboundMailDisabled: true},
	} {
		t.Run(cfg.Environment, func(t *testing.T) {
			var logs bytes.Buffer
			transport := newAccountMailer(cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
			err := transport.Send(context.Background(), mailer.Message{
				To: "private@example.test", Subject: "Verify account", Body: "private-verification-token",
			})
			if !errors.Is(err, mailer.ErrNotAccepted) {
				t.Fatalf("unconfigured transactional transport acknowledged delivery: %v", err)
			}
			if logs.Len() != 0 {
				t.Fatal("unconfigured transactional transport logged message data")
			}
		})
	}
}
