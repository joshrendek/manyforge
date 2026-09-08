package mailing

import (
	"errors"
	"reflect"
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
func TestSendingProfileRequiresFeedbackConfiguration(t *testing.T) {
	region := "us-east-1"
	configSet := "campaign-events"
	topic := "arn:aws:sns:us-west-2:123456789012:mailing-events"
	tests := []struct {
		name string
		in   SendingProfileInput
	}{
		{
			name: "SES configuration set",
			in: SendingProfileInput{
				Mode: "ses", FromEmail: "sender@example.com", FromName: "Sender",
				SES:       &SESCredentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"},
				SESRegion: &region, SNSTopicARN: &topic,
			},
		},
		{
			name: "SES topic region binding",
			in: SendingProfileInput{
				Mode: "ses", FromEmail: "sender@example.com", FromName: "Sender",
				SES:       &SESCredentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"},
				SESRegion: &region, SESConfigurationSet: &configSet, SNSTopicARN: &topic,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (&Service{}).PutSendingProfile(t.Context(), uuid.New(), uuid.New(), tt.in)
			if !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("PutSendingProfile error = %v, want validation", err)
			}
		})
	}
}

func TestResendCredentialsDoNotAcceptTenantWebhookSecret(t *testing.T) {
	if _, ok := reflect.TypeOf(ResendCredentials{}).FieldByName("WebhookSecret"); ok {
		t.Fatal("tenant-facing Resend credentials expose a webhook secret")
	}
}

func TestSafeProviderMessageTruncatesByRune(t *testing.T) {
	message := strings.Repeat("é", 501)
	got := safeProviderMessage(errors.New(message))
	if len([]rune(got)) != 500 || !strings.HasSuffix(got, "é") {
		t.Fatalf("safeProviderMessage rune length = %d", len([]rune(got)))
	}
}
