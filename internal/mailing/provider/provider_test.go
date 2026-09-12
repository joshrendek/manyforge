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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
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
		{name: "disabled", err: notify.ErrNotAccepted, status: "failed"},
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

func TestDisabledFactoryRejectsEveryTransport(t *testing.T) {
	domainID := uuid.New()
	factory := Factory{Disabled: true, RelaySender: notify.DisabledSender{}}
	for _, mode := range []string{"relay", "resend", "ses", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			deliverer, err := factory.Build(context.Background(), Profile{
				Mode: mode, EmailDomainID: &domainID, FromEmail: "sender@example.test",
				ResendAPIKey: "configured-resend-key", SESRegion: "us-east-1",
				SESAccessKeyID: "configured-access-key", SESSecretAccessKey: "configured-secret",
				SESConfigurationSet: "configured-set",
			})
			if !errors.Is(err, notify.ErrNotAccepted) || deliverer != nil {
				t.Fatalf("disabled %s build = %T, %v; want no client and non-acceptance", mode, deliverer, err)
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
			tags, ok := body["tags"].([]any)
			if !ok || len(tags) != 1 || tags[0].(map[string]any)["name"] != "mf_delivery" || tags[0].(map[string]any)["value"] != "delivery-id" {
				t.Errorf("tags = %#v", body["tags"])
			}
			_, _ = io.WriteString(w, `{"id":"resend-123"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	// API delivery must work without a configured shared SMTP sender.
	factory := &Factory{ResendBaseURL: server.URL, HTTPClient: server.Client()}
	r, err := factory.Build(context.Background(), Profile{
		Mode: "resend", ResendAPIKey: "re_test", FromEmail: "sender@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.(Verifier).Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	result, err := r.Send(context.Background(), notify.Mail{
		From: "Acme <sender@example.com>", To: "reader@example.net", Subject: "News",
		BodyText: "Hello", BodyHTML: "<p>Hello</p>", MessageID: "delivery@example.test",
		ExtraHeaders: map[string]string{"X-MF-Delivery": "delivery-id"},
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
	if strings.Contains(err.Error(), "slow down") {
		t.Fatalf("provider response body leaked through error: %q", err)
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

func TestResendEnsureWebhookCreatesExactFeedbackRoute(t *testing.T) {
	const endpoint = "https://hub.example.test/inbound/mailing/profile-a/resend"
	created, postCalls := false, 0
	var gotEndpoint string
	var gotEvents []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks":
			data := []any{}
			if created {
				data = append(data, map[string]any{"id": "wh_123", "status": "enabled", "endpoint": endpoint, "events": gotEvents})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "has_more": false, "data": data})
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks/wh_123":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "wh_123", "status": "enabled", "endpoint": endpoint, "events": gotEvents,
				"signing_secret": "whsec_MDEyMzQ1Njc4OWFiY2RlZg==",
			})
		case req.Method == http.MethodPost && req.URL.Path == "/webhooks":
			postCalls++
			var body struct {
				Endpoint string   `json:"endpoint"`
				Events   []string `json:"events"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			gotEndpoint, gotEvents, created = body.Endpoint, body.Events, true
			_, _ = io.WriteString(w, `{"object":"webhook","id":"wh_123","signing_secret":"whsec_MDEyMzQ1Njc4OWFiY2RlZg=="}`)
		default:
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	got, wasCreated, err := r.EnsureWebhook(context.Background(), endpoint, "", func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !wasCreated || postCalls != 1 || got.ID != "wh_123" || got.SigningSecret == "" {
		t.Fatalf("created webhook = %+v/%v posts=%d", got, wasCreated, postCalls)
	}
	if gotEndpoint != endpoint || !containsStrings(gotEvents, "email.bounced", "email.complained") {
		t.Fatalf("create payload endpoint=%q events=%v", gotEndpoint, gotEvents)
	}
}

func TestResendEnsureWebhookReconcilesLostCreateResponseWithoutSecondPost(t *testing.T) {
	const endpoint = "https://hub.example.test/inbound/mailing/profile-a/resend"
	created, postCalls := false, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks":
			data := []any{}
			if created {
				data = append(data, map[string]any{"id": "wh_lost", "status": "enabled", "endpoint": endpoint, "events": []string{"email.bounced", "email.complained"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"has_more": false, "data": data})
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks/wh_lost":
			_, _ = io.WriteString(w, `{"id":"wh_lost","status":"enabled","endpoint":"`+endpoint+`","events":["email.bounced","email.complained"],"signing_secret":"whsec_MDEyMzQ1Njc4OWFiY2RlZg=="}`)
		case req.Method == http.MethodPost && req.URL.Path == "/webhooks":
			postCalls++
			created = true
			panic(http.ErrAbortHandler)
		default:
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	got, _, err := r.EnsureWebhook(context.Background(), endpoint, "", func(context.Context) error { return nil })
	if err != nil || got.ID != "wh_lost" {
		t.Fatalf("lost-response reconciliation = %+v, err=%v", got, err)
	}
	if _, _, err = r.EnsureWebhook(context.Background(), endpoint, got.ID, func(context.Context) error {
		return errors.New("existing webhook recovery must remain read-only")
	}); err != nil {
		t.Fatal(err)
	}
	if postCalls != 1 {
		t.Fatalf("ambiguous create POST count = %d, want 1", postCalls)
	}
}

func TestResendEnsureWebhookRetainsReconciliationPathAfterLostResponseAndListFailure(t *testing.T) {
	const endpoint = "https://hub.example.test/inbound/mailing/profile-a/resend"
	listCalls, postCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks":
			listCalls++
			switch listCalls {
			case 1:
				_, _ = io.WriteString(w, `{"has_more":false,"data":[]}`)
			case 2:
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			default:
				_, _ = io.WriteString(w, `{"has_more":false,"data":[{"id":"wh_recovered","status":"enabled","endpoint":"`+endpoint+`","events":["email.bounced","email.complained"]}]}`)
			}
		case req.Method == http.MethodPost && req.URL.Path == "/webhooks":
			postCalls++
			panic(http.ErrAbortHandler)
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks/wh_recovered":
			_, _ = io.WriteString(w, `{"id":"wh_recovered","status":"enabled","endpoint":"`+endpoint+`","events":["email.bounced","email.complained"],"signing_secret":"whsec_MDEyMzQ1Njc4OWFiY2RlZg=="} `)
		default:
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	if _, _, err := r.EnsureWebhook(context.Background(), endpoint, "", func(context.Context) error { return nil }); err == nil {
		t.Fatal("lost create response plus failed reconciliation was acknowledged")
	}
	got, _, err := r.EnsureWebhook(context.Background(), endpoint, "", func(context.Context) error {
		return errors.New("ambiguous webhook recovery must remain read-only")
	})
	if err != nil || got.ID != "wh_recovered" {
		t.Fatalf("durable retry reconciliation = %+v, err=%v", got, err)
	}
	if postCalls != 1 {
		t.Fatalf("ambiguous create POST count = %d, want 1", postCalls)
	}
}

func TestResendEnsureWebhookCanonicalizesDuplicateExactEndpoints(t *testing.T) {
	const endpoint = "https://hub.example.test/inbound/mailing/profile-a/resend"
	hooks := map[string]bool{"wh_a": true, "wh_b": true, "wh_c": true}
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks":
			data := make([]map[string]any, 0, len(hooks))
			for id := range hooks {
				data = append(data, map[string]any{"id": id, "status": "enabled", "endpoint": endpoint, "events": []string{"email.bounced", "email.complained"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"has_more": false, "data": data})
		case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/webhooks/"):
			id := strings.TrimPrefix(req.URL.Path, "/webhooks/")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": id, "status": "enabled", "endpoint": endpoint,
				"events":         []string{"email.bounced", "email.complained"},
				"signing_secret": "whsec_" + base64.StdEncoding.EncodeToString([]byte(id+"-0123456789abcdef")),
			})
		case req.Method == http.MethodDelete:
			id := strings.TrimPrefix(req.URL.Path, "/webhooks/")
			deleted = append(deleted, id)
			delete(hooks, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	got, created, err := r.EnsureWebhook(context.Background(), endpoint, "", func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if created || got.ID != "wh_a" || len(deleted) != 2 || len(hooks) != 1 {
		t.Fatalf("canonical webhook=%+v created=%v deleted=%v remaining=%v", got, created, deleted, hooks)
	}
}

func TestResendEnsureWebhookRejectsWrongExistingRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks":
			_, _ = io.WriteString(w, `{"has_more":false,"data":[{"id":"wh_123","status":"enabled","endpoint":"https://attacker.test/hook","events":["email.bounced","email.complained"]}]}`)
		case req.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"id":"wh_123","status":"enabled","endpoint":"https://attacker.test/hook","events":["email.bounced","email.complained"],"signing_secret":"whsec_MDEyMzQ1Njc4OWFiY2RlZg=="}`)
		case req.Method == http.MethodDelete:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	if _, _, err := r.EnsureWebhook(context.Background(), "https://hub.example.test/inbound/mailing/profile-a/resend", "wh_123", func(context.Context) error { return nil }); err == nil {
		t.Fatal("accepted or ignored wrong existing Resend feedback route cleanup failure")
	}
}

func TestResendEnsureWebhookGuardsEveryRemoteMutation(t *testing.T) {
	const endpoint = "https://hub.example.test/inbound/mailing/profile-a/resend"
	denied := errors.New("provisioning lease no longer held")
	for _, tc := range []struct {
		name       string
		initial    string
		denyAt     int32
		nilGuard   bool
		wantWrites int32
		wantID     string
	}{
		{name: "create", wantWrites: 1, wantID: "wh_created"},
		{name: "denied create", denyAt: 1},
		{name: "missing required guard", nilGuard: true},
		{name: "replace invalid hook", initial: "invalid", wantWrites: 2, wantID: "wh_created"},
		{name: "denied invalid hook delete", initial: "invalid", denyAt: 1},
		{name: "denied create after invalid hook delete", initial: "invalid", denyAt: 2, wantWrites: 1},
		{name: "delete duplicates", initial: "duplicates", wantWrites: 2, wantID: "wh_a"},
		{name: "denied second duplicate delete", initial: "duplicates", denyAt: 2, wantWrites: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := map[string]bool{}
			switch tc.initial {
			case "invalid":
				hooks["wh_invalid"] = false
			case "duplicates":
				hooks["wh_a"], hooks["wh_b"], hooks["wh_c"] = true, true, true
			}
			var guardCalls, writeCalls atomic.Int32
			var guarded atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodGet {
					if guarded.Load() {
						t.Error("mutation guard was called before a read instead of immediately before a write")
					}
				} else {
					writeCalls.Add(1)
					if !guarded.CompareAndSwap(true, false) {
						t.Error("remote mutation had no fresh successful guard")
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}
				switch {
				case req.Method == http.MethodGet && req.URL.Path == "/webhooks":
					data := make([]map[string]any, 0, len(hooks))
					for id := range hooks {
						data = append(data, map[string]any{"id": id, "endpoint": endpoint})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false})
				case req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/webhooks/"):
					id := strings.TrimPrefix(req.URL.Path, "/webhooks/")
					status := "enabled"
					if !hooks[id] {
						status = "disabled"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": id, "endpoint": endpoint, "status": status,
						"events":         []string{"email.bounced", "email.complained"},
						"signing_secret": "whsec_MDEyMzQ1Njc4OWFiY2RlZg==",
					})
				case req.Method == http.MethodPost && req.URL.Path == "/webhooks":
					hooks["wh_created"] = true
					_, _ = io.WriteString(w, `{"id":"wh_created","signing_secret":"whsec_MDEyMzQ1Njc4OWFiY2RlZg=="}`)
				case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/webhooks/"):
					delete(hooks, strings.TrimPrefix(req.URL.Path, "/webhooks/"))
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected provider request: %s %s", req.Method, req.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			beforeMutation := func(context.Context) error {
				call := guardCalls.Add(1)
				if call == tc.denyAt {
					return denied
				}
				if !guarded.CompareAndSwap(false, true) {
					t.Error("guard called again without an intervening remote mutation")
				}
				return nil
			}
			if tc.nilGuard {
				beforeMutation = nil
			}
			r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
			got, _, err := r.EnsureWebhook(context.Background(), endpoint, "", beforeMutation)
			switch {
			case tc.nilGuard:
				if !errors.Is(err, ErrProviderConfiguration) || guardCalls.Load() != 0 {
					t.Fatalf("missing guard error=%v calls=%d", err, guardCalls.Load())
				}
			case tc.denyAt != 0:
				if !errors.Is(err, denied) || guardCalls.Load() != tc.denyAt {
					t.Fatalf("denied mutation error=%v guard_calls=%d", err, guardCalls.Load())
				}
			default:
				if err != nil || got.ID != tc.wantID || guardCalls.Load() != tc.wantWrites {
					t.Fatalf("guarded reconciliation webhook=%+v err=%v guard_calls=%d", got, err, guardCalls.Load())
				}
			}
			if writeCalls.Load() != tc.wantWrites || guarded.Load() {
				t.Fatalf("remote writes=%d want=%d unused_guard=%v", writeCalls.Load(), tc.wantWrites, guarded.Load())
			}
		})
	}
}

func TestResendCleanupWebhooksDeletesAllExactEndpointHooks(t *testing.T) {
	const endpoint = "https://hub.example.test/inbound/mailing/profile-a/resend"
	var deleted []string
	listCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks":
			listCalls++
			if len(deleted) == 0 {
				_, _ = io.WriteString(w, `{"has_more":false,"data":[
					{"id":"wh_b","endpoint":"`+endpoint+`"},
					{"id":"wh_a","endpoint":"`+endpoint+`"},
					{"id":"wh_other","endpoint":"https://other.example.test/resend"}]}`)
			} else {
				_, _ = io.WriteString(w, `{"has_more":false,"data":[
					{"id":"wh_other","endpoint":"https://other.example.test/resend"}]}`)
			}
		case req.Method == http.MethodDelete:
			deleted = append(deleted, strings.TrimPrefix(req.URL.Path, "/webhooks/"))
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	if err := r.CleanupWebhooks(context.Background(), endpoint, "", true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"wh_b", "wh_a"}) || listCalls != 2 {
		t.Fatalf("replacement cleanup deleted=%v list_calls=%d", deleted, listCalls)
	}
}

func TestResendCleanupWebhooksRejectsEmptyReplacementAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || req.URL.Path != "/webhooks" {
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
		_, _ = io.WriteString(w, `{"has_more":false,"data":[]}`)
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_other_account", BaseURL: server.URL, Client: server.Client()}
	if err := r.CleanupWebhooks(context.Background(),
		"https://hub.example.test/inbound/mailing/profile-a/resend", "wh_expected", true); err == nil {
		t.Fatal("empty replacement-account list accepted as cleanup proof")
	}
	if err := r.CleanupWebhooks(context.Background(),
		"https://hub.example.test/inbound/mailing/profile-a/resend", "", false); err != nil {
		t.Fatalf("retained account-bound key could not prove exact-endpoint absence: %v", err)
	}
}

func containsStrings(values []string, wants ...string) bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	for _, want := range wants {
		if !set[want] {
			return false
		}
	}
	return true
}

func TestSESEndpointResolverSendAndVerify(t *testing.T) {
	configurationChecked := false
	eventDestinationsJSON := `{"EventDestinations":[{"Name":"feedback","Enabled":true,"MatchingEventTypes":["BOUNCE","COMPLAINT"],"SnsDestination":{"TopicArn":"arn:aws:sns:us-east-1:123456789012:mailing-events"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/email/identities/example.com":
			_, _ = io.WriteString(w, `{"VerifiedForSendingStatus":true}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/email/account":
			_, _ = io.WriteString(w, `{"SendingEnabled":true}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/email/configuration-sets/campaign-events":
			configurationChecked = true
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/email/configuration-sets/campaign-events/event-destinations":
			_, _ = io.WriteString(w, eventDestinationsJSON)
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
		SNSTopicARN: "arn:aws:sns:us-east-1:123456789012:mailing-events",
	}
	endpointURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := NewSES(context.Background(), profile, testSESEndpointResolver{url: endpointURL}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	sender.Identity = stubSTSIdentity{accountID: "123456789012"}
	if err := sender.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !configurationChecked {
		t.Fatal("Verify did not validate the configured SES event configuration set")
	}
	for name, response := range map[string]string{
		"no destination":    `{"EventDestinations":[]}`,
		"disabled":          `{"EventDestinations":[{"Name":"feedback","Enabled":false,"MatchingEventTypes":["BOUNCE","COMPLAINT"],"SnsDestination":{"TopicArn":"arn:aws:sns:us-east-1:123456789012:mailing-events"}}]}`,
		"wrong topic":       `{"EventDestinations":[{"Name":"feedback","Enabled":true,"MatchingEventTypes":["BOUNCE","COMPLAINT"],"SnsDestination":{"TopicArn":"arn:aws:sns:us-east-1:123456789012:other"}}]}`,
		"missing bounce":    `{"EventDestinations":[{"Name":"feedback","Enabled":true,"MatchingEventTypes":["COMPLAINT"],"SnsDestination":{"TopicArn":"arn:aws:sns:us-east-1:123456789012:mailing-events"}}]}`,
		"missing complaint": `{"EventDestinations":[{"Name":"feedback","Enabled":true,"MatchingEventTypes":["BOUNCE"],"SnsDestination":{"TopicArn":"arn:aws:sns:us-east-1:123456789012:mailing-events"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			eventDestinationsJSON = response
			if err := sender.Verify(context.Background()); !errors.Is(err, ErrSESFeedback) {
				t.Fatalf("Verify did not identify the invalid SES feedback route: %v", err)
			}
		})
	}
	eventDestinationsJSON = `{"EventDestinations":[{"Name":"feedback","Enabled":true,"MatchingEventTypes":["BOUNCE","COMPLAINT"],"SnsDestination":{"TopicArn":"arn:aws:sns:us-east-1:123456789012:mailing-events"}}]}`
	sender.Identity = stubSTSIdentity{accountID: "999999999999"}
	if err := sender.Verify(context.Background()); !errors.Is(err, ErrSESFeedback) {
		t.Fatalf("Verify did not identify the SNS credential account mismatch: %v", err)
	}
	sender.Identity = stubSTSIdentity{accountID: "123456789012"}
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

type stubSTSIdentity struct{ accountID string }

func (s stubSTSIdentity) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String(s.accountID)}, nil
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

func TestCacheConcurrentResolve(t *testing.T) {
	cache := NewCache(func(context.Context, Profile) (Deliverer, error) {
		return stubDeliverer{}, nil
	}, time.Minute)
	p := Profile{ID: uuid.New(), UpdatedAt: time.Now()}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.Resolve(context.Background(), p)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(cache.entries); got != 1 {
		t.Fatalf("cache entries = %d, want 1", got)
	}
}

func TestRelayRequiresCompleteConfiguration(t *testing.T) {
	r := &Relay{}
	if err := r.Verify(context.Background()); !errors.Is(err, ErrRelayConfiguration) {
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
