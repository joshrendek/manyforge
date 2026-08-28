package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"
	smithyendpoints "github.com/aws/smithy-go/endpoints"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/notify"
)

func TestClassify(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		err      error
		attempts int
		status   string
		retry    bool
		delay    time.Duration
	}{
		{name: "sent", status: "sent"},
		{name: "suppressed", err: notify.ErrSuppressed, status: "suppressed"},
		{name: "bad request", err: &HTTPError{StatusCode: 400}, status: "failed"},
		{name: "unauthorized", err: &HTTPError{StatusCode: 401}, status: "failed"},
		{name: "forbidden", err: &HTTPError{StatusCode: 403}, status: "failed"},
		{name: "not found", err: &HTTPError{StatusCode: 404}, status: "failed"},
		{name: "unprocessable", err: &HTTPError{StatusCode: 422}, status: "failed"},
		{name: "SES rejected", err: &types.MessageRejected{}, status: "failed"},
		{name: "SES suspended", err: &types.AccountSuspendedException{}, status: "failed"},
		{name: "rate limit", err: &HTTPError{StatusCode: 429}, status: "retry", retry: true, delay: 30 * time.Second},
		{name: "server", err: &HTTPError{StatusCode: 503}, attempts: 2, status: "retry", retry: true, delay: 2 * time.Minute},
		{name: "network", err: &net.DNSError{Err: "temporary", IsTemporary: true}, attempts: 1, status: "retry", retry: true, delay: time.Minute},
		{name: "smithy server", err: &smithy.GenericAPIError{Code: "InternalFailure", Message: "retry", Fault: smithy.FaultServer}, attempts: 1, status: "retry", retry: true, delay: time.Minute},
		{name: "attempt cap", err: &HTTPError{StatusCode: 503}, attempts: 5, status: "failed"},
		{name: "unknown", err: errors.New("boom"), status: "failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err, tc.attempts, now)
			if got.Status != tc.status || got.Retry != tc.retry {
				t.Fatalf("Classify = %+v, want status=%s retry=%v", got, tc.status, tc.retry)
			}
			if tc.retry && got.NotBefore.Sub(now) != tc.delay {
				t.Fatalf("delay = %s, want %s", got.NotBefore.Sub(now), tc.delay)
			}
		})
	}
}

func TestResendSendAndVerify(t *testing.T) {
	var sent bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer re_test" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/domains":
			_, _ = io.WriteString(w, `{"data":[{"name":"example.com","status":"verified"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/emails":
			sent = true
			if got := r.Header.Get("Idempotency-Key"); got != "delivery@example.test" {
				t.Errorf("idempotency key = %q", got)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["html"] != "<p>Hello</p>" || body["text"] != "Hello" {
				t.Errorf("body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"id":"resend-123"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	r := &Resend{APIKey: "re_test", FromEmail: "sender@example.com", BaseURL: server.URL, Client: server.Client()}
	if err := r.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	result, err := r.Send(context.Background(), notify.Mail{
		From: "Acme <sender@example.com>", To: "reader@example.net", Subject: "News",
		BodyText: "Hello", BodyHTML: "<p>Hello</p>", MessageID: "delivery@example.test",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !sent || result.ProviderID != "resend-123" {
		t.Fatalf("result = %+v, sent=%v", result, sent)
	}
}

func TestResendHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"name":"rate_limit_exceeded","message":"slow down"}`)
	}))
	defer server.Close()
	r := &Resend{APIKey: "x", FromEmail: "x@example.com", BaseURL: server.URL, Client: server.Client()}
	_, err := r.Send(context.Background(), notify.Mail{From: "x@example.com", To: "y@example.com"})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 429 || httpErr.Code != "rate_limit_exceeded" {
		t.Fatalf("error = %#v", err)
	}
}

func TestResendRejectsHeaderInjectionBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	r := &Resend{APIKey: "x", FromEmail: "x@example.com", BaseURL: server.URL, Client: server.Client()}
	_, err := r.Send(context.Background(), notify.Mail{
		From: "x@example.com", To: "y@example.com", Subject: "hello\r\nBcc: victim@example.com",
	})
	if err == nil || calls.Load() != 0 {
		t.Fatalf("Send error = %v, network calls = %d", err, calls.Load())
	}
}

func TestSESEndpointResolverSendAndVerify(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/email/identities/example.com":
			_, _ = io.WriteString(w, `{"VerifiedForSendingStatus":true}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/email/account":
			_, _ = io.WriteString(w, `{"SendingEnabled":true}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/email/outbound-emails":
			var body struct {
				Content struct {
					Raw struct{ Data string }
				}
				ConfigurationSetName string
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			raw, err := base64.StdEncoding.DecodeString(body.Content.Raw.Data)
			if err != nil || !strings.Contains(string(raw), "Subject: News\r\n") {
				t.Errorf("raw MIME = %q, err=%v", raw, err)
			}
			if body.ConfigurationSetName != "campaign-events" {
				t.Errorf("configuration set = %q", body.ConfigurationSetName)
			}
			_, _ = io.WriteString(w, `{"MessageId":"ses-123"}`)
		default:
			http.Error(w, fmt.Sprintf("unexpected %s %s", r.Method, r.URL.Path), http.StatusNotFound)
		}
	}))
	defer server.Close()

	profile := Profile{
		FromEmail: "sender@example.com", SESRegion: "us-east-1",
		SESAccessKeyID: "AKID", SESSecretAccessKey: "secret", SESConfigurationSet: "campaign-events",
	}
	endpointURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewSES(context.Background(), profile, testSESEndpointResolver{url: endpointURL}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	result, err := sender.Send(context.Background(), notify.Mail{
		From: "sender@example.com", To: "reader@example.net", Subject: "News",
		BodyText: "Hello", MessageID: "delivery@example.com",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.ProviderID != "ses-123" {
		t.Fatalf("provider id = %q", result.ProviderID)
	}
}

type testSESEndpointResolver struct{ url *url.URL }

func (r testSESEndpointResolver) ResolveEndpoint(context.Context, sesv2.EndpointParameters) (smithyendpoints.Endpoint, error) {
	return smithyendpoints.Endpoint{URI: *r.url}, nil
}

type stubDeliverer struct{}

func (stubDeliverer) Send(context.Context, notify.Mail) (SendResult, error) { return SendResult{}, nil }

func TestCacheKeysByProfileAndUpdatedAt(t *testing.T) {
	var builds atomic.Int32
	cache := NewCache(func(context.Context, Profile) (Deliverer, error) {
		builds.Add(1)
		return stubDeliverer{}, nil
	}, time.Minute)
	id := uuid.New()
	p := Profile{ID: id, UpdatedAt: time.Unix(1, 0)}
	for range 2 {
		if _, err := cache.Resolve(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	p.UpdatedAt = p.UpdatedAt.Add(time.Second)
	if _, err := cache.Resolve(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("build count = %d, want 2", got)
	}
	if got := len(cache.entries); got != 1 {
		t.Fatalf("cache entries = %d, want 1", got)
	}
}

func TestCacheExpirationAndBuildError(t *testing.T) {
	now := time.Unix(10, 0)
	var builds atomic.Int32
	cache := NewCache(func(context.Context, Profile) (Deliverer, error) {
		if builds.Add(1) == 3 {
			return nil, errors.New("build failed")
		}
		return stubDeliverer{}, nil
	}, time.Minute)
	cache.now = func() time.Time { return now }
	p := Profile{ID: uuid.New(), UpdatedAt: now}
	if _, err := cache.Resolve(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := cache.Resolve(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.UpdatedAt = p.UpdatedAt.Add(time.Second)
	if _, err := cache.Resolve(context.Background(), p); err == nil || err.Error() != "build failed" {
		t.Fatalf("build error = %v", err)
	}
}

func TestRelayRequiresCompleteConfiguration(t *testing.T) {
	r := &Relay{}
	if err := r.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("Verify error = %v", err)
	}
}

func TestNewSESValidatesStaticConfiguration(t *testing.T) {
	tests := []Profile{
		{SESAccessKeyID: "id", SESSecretAccessKey: "secret"},
		{SESRegion: "us-east-1", SESSecretAccessKey: "secret"},
		{SESRegion: "us-east-1", SESAccessKeyID: "id"},
	}
	for _, profile := range tests {
		if _, err := NewSES(context.Background(), profile, nil, nil); err == nil {
			t.Fatalf("NewSES(%+v) succeeded", profile)
		}
	}
}
