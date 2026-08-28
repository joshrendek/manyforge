package mailing

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestPreviewRejectsOversizedMarkdownBeforeDatabaseAccess(t *testing.T) {
	svc := &Service{}
	_, err := svc.Preview(t.Context(), uuid.New(), uuid.New(), PreviewInput{BodyMarkdown: strings.Repeat("a", (1<<20)+1)})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("Preview error = %v, want validation", err)
	}
}

func TestSendingProfileRejectsInvalidFromName(t *testing.T) {
	tests := []string{"", "Sender\r\nBcc: victim@example.com", strings.Repeat("a", 201)}
	for _, fromName := range tests {
		svc := &Service{}
		_, err := svc.PutSendingProfile(t.Context(), uuid.New(), uuid.New(), SendingProfileInput{
			Mode: "resend", FromEmail: "sender@example.com", FromName: fromName,
		})
		if !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("from_name %q error = %v, want validation", fromName, err)
		}
	}
}

func TestSafeProviderMessageTruncatesByRune(t *testing.T) {
	message := strings.Repeat("é", 501)
	got := safeProviderMessage(errors.New(message))
	if len([]rune(got)) != 500 || !strings.HasSuffix(got, "é") {
		t.Fatalf("safeProviderMessage rune length = %d", len([]rune(got)))
	}
}
