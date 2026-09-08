package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/manyforge/manyforge/internal/platform/notify"
)

func TestVerificationMessageRejectsUntrustedErrorText(t *testing.T) {
	const hostile = "private-token https://internal.invalid/path?secret=credential"
	for name, err := range map[string]error{
		"unknown":             errors.New(hostile),
		"unknown HTTP code":   &HTTPError{StatusCode: 422, Code: hostile, Message: hostile},
		"unknown AWS code":    &sesRequestError{operation: hostile, cause: &smithy.GenericAPIError{Code: hostile, Message: hostile}},
		"HTTP authentication": &HTTPError{StatusCode: 401, Code: hostile, Message: hostile},
		"AWS authentication":  &sesRequestError{operation: "identity verification", cause: &smithy.GenericAPIError{Code: "InvalidClientTokenId", Message: hostile}},
		"network URL":         &url.Error{Op: "Get", URL: hostile, Err: &net.DNSError{Name: hostile, Err: hostile}},
		"wrapped feedback":    fmt.Errorf("%s: %w: %w", hostile, ErrResendWebhook, errors.New(hostile)),
	} {
		t.Run(name, func(t *testing.T) {
			message := VerificationMessage(err)
			for _, secret := range []string{"private-token", "internal.invalid", "secret=credential", "https://"} {
				if strings.Contains(message, secret) {
					t.Fatalf("verification disclosed untrusted payload: %q", message)
				}
			}
			if len(message) > 500 {
				t.Fatalf("unbounded diagnostic: %d bytes", len(message))
			}
		})
	}
}

func TestVerificationRemediationDistinguishesFailureBoundaries(t *testing.T) {
	// Assert actionable concepts, not exact prose. Authentication, permission,
	// identity and deployment failures must not all collapse to one fallback.
	cases := []struct {
		name   string
		err    error
		action string
	}{
		{"disabled", fmt.Errorf("wrapped: %w", notify.ErrNotAccepted), "MANYFORGE_OUTBOUND_MAIL_DISABLED"},
		{"Resend invalid key is not permission denial", &HTTPError{StatusCode: 403, Code: "invalid_api_key"}, "Replace"},
		{"Resend restricted key", &HTTPError{StatusCode: 403, Code: "restricted_api_key"}, "full-access"},
		{"SES invalid credential is not permission denial", &sesRequestError{operation: "identity verification", cause: &smithy.GenericAPIError{Code: "InvalidClientTokenId"}}, "Replace"},
		{"SES denied", &sesRequestError{operation: "identity verification", cause: &smithy.GenericAPIError{Code: "AccessDeniedException"}}, "ses:GetEmailIdentity"},
		{"SES absent identity", &sesRequestError{operation: "identity verification", cause: &smithy.GenericAPIError{Code: "NotFoundException"}}, "DNS"},
		{"SES absent configuration set", &sesRequestError{operation: "configuration-set verification", cause: &smithy.GenericAPIError{Code: "NotFoundException"}}, "Create a configuration set"},
		{"SES sending disabled", ErrSESSendingDisabled, "restore SES sending"},
		{"SNS mismatch", ErrSESFeedback, "same region and AWS account"},
		{"webhook reconciliation", fmt.Errorf("%w: %w", ErrResendWebhook, errors.New("unknown")), "reconcile"},
		{"rate limit precedes webhook fallback", fmt.Errorf("%w: %w", ErrResendWebhook, &HTTPError{StatusCode: 429}), "Wait"},
		{"timeout", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "connectivity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := VerificationMessage(tc.err)
			if !strings.Contains(message, tc.action) {
				t.Fatalf("failure lost its remediation %q: %q", tc.action, message)
			}
		})
	}
}

func TestVerificationWrappersPreserveDeliveryClassification(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, cause := range []error{
		&HTTPError{StatusCode: 429},
		&HTTPError{StatusCode: 401},
		&smithy.GenericAPIError{Code: "InternalFailure", Fault: smithy.FaultServer},
	} {
		wrapped := fmt.Errorf("%w: %w", ErrSESFeedback, &sesRequestError{operation: "event-destination verification", cause: cause})
		if !errors.Is(wrapped, cause) || Classify(wrapped, 1, now) != Classify(cause, 1, now) {
			t.Fatalf("verification wrapper changed retry/terminal contract for %T", cause)
		}
	}
}

func TestResendVerificationReportsSenderDomainWithoutProviderPayload(t *testing.T) {
	for name, response := range map[string]string{
		"missing":    `{"data":[]}`,
		"unverified": `{"data":[{"name":"example.test","status":"private-provider-detail"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.URL.Path != "/domains" {
					t.Errorf("verification attempted unexpected provider operation: %s %s", req.Method, req.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_, _ = io.WriteString(w, response)
			}))
			defer server.Close()
			sender := &Resend{FromEmail: "sender@example.test", BaseURL: server.URL, Client: server.Client()}
			err := sender.Verify(context.Background())
			if !errors.Is(err, ErrSenderDomain) || strings.Contains(VerificationMessage(err), "private-provider-detail") {
				t.Fatalf("missing/unverified identity diagnostic = %v", err)
			}
		})
	}
}

func TestSESVerificationDistinguishesUnverifiedIdentityAndDisabledSending(t *testing.T) {
	identityVerified := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/v2/email/identities/example.test":
			_, _ = fmt.Fprintf(w, `{"VerifiedForSendingStatus":%t}`, identityVerified)
		case "/v2/email/account":
			_, _ = io.WriteString(w, `{"SendingEnabled":false}`)
		default:
			t.Errorf("verification continued after a failed prerequisite: %s", req.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewSES(context.Background(), Profile{
		FromEmail: "sender@example.test", SESRegion: "us-east-1",
		SESAccessKeyID: "test", SESSecretAccessKey: "test", SESConfigurationSet: "events",
	}, testSESEndpointResolver{url: endpoint}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Verify(context.Background()); !errors.Is(err, ErrSenderDomain) {
		t.Fatalf("unverified SES identity = %v", err)
	}
	identityVerified = true
	if err = sender.Verify(context.Background()); !errors.Is(err, ErrSESSendingDisabled) {
		t.Fatalf("disabled SES account = %v", err)
	}
}
