// Package provider resolves mailing sending profiles into provider-specific
// delivery clients while keeping campaign workers transport-agnostic.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/notify"
)

type Deliverer interface {
	Send(context.Context, notify.Mail) (SendResult, error)
}

type Verifier interface{ Verify(context.Context) error }

type SendResult struct {
	ProviderID string
}

type Profile struct {
	ID                  uuid.UUID
	UpdatedAt           time.Time
	Mode                string
	FromEmail           string
	EmailDomainID       *uuid.UUID
	SESRegion           string
	SESConfigurationSet string
	ResendAPIKey        string
	SESAccessKeyID      string
	SESSecretAccessKey  string
}

type HTTPError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("provider: http %d %s: %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("provider: http %d: %s", e.StatusCode, e.Message)
}

type Classification struct {
	Status    string
	Retry     bool
	NotBefore time.Time
}

// Classify turns transport-specific errors into the delivery worker's stable
// terminal/retry contract. attempts is the number of attempts already made.
func Classify(err error, attempts int, now time.Time) Classification {
	if err == nil {
		return Classification{Status: "sent"}
	}
	if errors.Is(err, notify.ErrSuppressed) {
		return Classification{Status: "suppressed"}
	}
	terminal := false
	retryable := false
	var httpErr *HTTPError
	var responseErr *smithyhttp.ResponseError
	var apiErr smithy.APIError
	var netErr net.Error
	switch {
	case errors.As(err, &httpErr):
		retryable = httpErr.StatusCode == 429 || httpErr.StatusCode >= 500
		terminal = httpErr.StatusCode >= 400 && httpErr.StatusCode < 500 && httpErr.StatusCode != 429
	case errors.As(err, &responseErr):
		status := responseErr.HTTPStatusCode()
		retryable = status == 429 || status >= 500
		terminal = status >= 400 && status < 500 && status != 429
	case errors.As(err, &apiErr):
		switch apiErr.ErrorCode() {
		case "MessageRejected", "AccountSuspendedException", "BadRequestException", "NotFoundException", "MailFromDomainNotVerifiedException":
			terminal = true
		default:
			retryable = apiErr.ErrorFault() == smithy.FaultServer
		}
	case errors.As(err, &netErr):
		retryable = true
	}
	var rejected *types.MessageRejected
	var suspended *types.AccountSuspendedException
	if errors.As(err, &rejected) || errors.As(err, &suspended) {
		terminal = true
		retryable = false
	}
	if terminal || attempts >= 5 || !retryable {
		return Classification{Status: "failed"}
	}
	delay := 30 * time.Second
	for i := 0; i < attempts && delay < 30*time.Minute; i++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	return Classification{Status: "retry", Retry: true, NotBefore: now.Add(delay)}
}
