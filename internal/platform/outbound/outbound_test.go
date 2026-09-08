package outbound

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	smithyendpoints "github.com/aws/smithy-go/endpoints"

	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/platform/mailer"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

const secretMarker = "private-token-must-not-escape"

type capturedMail struct {
	header               mail.Header
	text, html, envelope string
	configurationSet     string
}

type wireCapture struct {
	messages chan capturedMail
	requests atomic.Int32
}

func (c *wireCapture) next(t *testing.T) capturedMail {
	t.Helper()
	select {
	case message := <-c.messages:
		return message
	case <-time.After(3 * time.Second):
		t.Fatal("mail did not reach the local transport")
		return capturedMail{}
	}
}

func decodeMIME(t *testing.T, raw []byte) capturedMail {
	t.Helper()
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Errorf("read MIME: %v", err)
		return capturedMail{}
	}
	captured := capturedMail{header: message.Header}
	mediaType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil {
		t.Errorf("parse MIME content type: %v", err)
		return captured
	}
	if mediaType != "multipart/alternative" {
		body, _ := io.ReadAll(message.Body)
		captured.text = strings.TrimSuffix(string(body), "\r\n")
		return captured
	}
	reader := multipart.NewReader(message.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Errorf("read MIME part: %v", err)
			break
		}
		body, _ := io.ReadAll(part)
		if strings.HasPrefix(part.Header.Get("Content-Type"), "text/html") {
			captured.html = string(body)
		} else {
			captured.text = string(body)
		}
	}
	return captured
}

func smtpCapture(t *testing.T, reject bool) (*wireCapture, string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	capture := &wireCapture{messages: make(chan capturedMail, 8)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			capture.requests.Add(1)
			func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				reader := textproto.NewReader(bufio.NewReader(conn))
				_, _ = fmt.Fprint(conn, "220 localhost ESMTP\r\n")
				envelope := ""
				for {
					line, err := reader.ReadLine()
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
						_, _ = fmt.Fprint(conn, "250 localhost\r\n")
					case strings.HasPrefix(line, "MAIL FROM:"):
						if reject {
							_, _ = fmt.Fprintf(conn, "550 %s\r\n", secretMarker)
							return
						}
						envelope = strings.TrimPrefix(line, "MAIL FROM:")
						_, _ = fmt.Fprint(conn, "250 OK\r\n")
					case strings.HasPrefix(line, "RCPT TO:"):
						_, _ = fmt.Fprint(conn, "250 OK\r\n")
					case line == "DATA":
						_, _ = fmt.Fprint(conn, "354 send mail\r\n")
						raw, err := reader.ReadDotBytes()
						if err != nil {
							return
						}
						message := decodeMIME(t, bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n")))
						message.envelope = envelope
						capture.messages <- message
						_, _ = fmt.Fprint(conn, "250 queued\r\n")
					case line == "QUIT":
						_, _ = fmt.Fprint(conn, "221 bye\r\n")
						return
					default:
						_, _ = fmt.Fprint(conn, "500 unsupported\r\n")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	return capture, host, portNumber
}

type localSESEndpoint struct{ url url.URL }

func (e localSESEndpoint) ResolveEndpoint(context.Context, sesv2.EndpointParameters) (smithyendpoints.Endpoint, error) {
	return smithyendpoints.Endpoint{URI: e.url}, nil
}

func localTransport(t *testing.T, mode string, reject bool) (config.Config, Dependencies, *wireCapture) {
	t.Helper()
	cfg := config.Config{
		Environment: "production", OutboundProvider: mode,
		OutboundFromEmail: "accounts@system.example", OutboundFromName: "System Accounts",
		OutboundResendAPIKey: "local-resend-key", OutboundSESRegion: "us-east-1",
		OutboundSESAccessKeyID: "local-access-key", OutboundSESSecretAccessKey: "local-secret-key",
	}
	deps := Dependencies{}
	if mode == "smtp" {
		capture, host, port := smtpCapture(t, reject)
		cfg.SMTPHost, cfg.SMTPPort = host, port
		return cfg, deps, capture
	}
	capture := &wireCapture{messages: make(chan capturedMail, 8)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.requests.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("unexpected provider method %s", r.Method)
		}
		if mode == "resend" {
			if r.URL.Path != "/emails" || r.Header.Get("Authorization") != "Bearer local-resend-key" {
				t.Error("Resend request did not use the send endpoint and configured credential")
			}
		} else if r.URL.Path != "/v2/email/outbound-emails" || !strings.Contains(r.Header.Get("Authorization"), "Credential=local-access-key/") || !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/ses/aws4_request") {
			t.Error("SES request did not use the send endpoint and configured signing identity")
		}
		w.Header().Set("Content-Type", "application/json")
		if reject {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"name":%q,"message":%q,"__type":"BadRequestException"}`, secretMarker, secretMarker)
			return
		}
		var message capturedMail
		if mode == "resend" {
			var payload struct {
				From, Subject, Text, HTML string
				To                        []string
				ReplyTo                   string `json:"reply_to"`
				Headers                   map[string]string
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode Resend payload: %v", err)
				return
			}
			header := make(mail.Header)
			for key, value := range payload.Headers {
				header[textproto.CanonicalMIMEHeaderKey(key)] = []string{value}
			}
			header["From"], header["To"] = []string{payload.From}, payload.To
			header["Subject"], header["Reply-To"] = []string{payload.Subject}, []string{payload.ReplyTo}
			message = capturedMail{header: header, text: payload.Text, html: payload.HTML}
			_, _ = fmt.Fprint(w, `{"id":"resend-accepted"}`)
		} else {
			var payload struct {
				Content              struct{ Raw struct{ Data []byte } }
				ConfigurationSetName string
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode SES payload: %v", err)
				return
			}
			message = decodeMIME(t, payload.Content.Raw.Data)
			message.configurationSet = payload.ConfigurationSetName
			_, _ = fmt.Fprint(w, `{"MessageId":"ses-accepted"}`)
		}
		capture.messages <- message
	}))
	t.Cleanup(server.Close)
	deps.HTTPClient = server.Client()
	deps.ResendBaseURL = server.URL
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	deps.SESEndpointResolver = localSESEndpoint{url: *endpoint}
	return cfg, deps, capture
}

func supportMessage() notify.Mail {
	return notify.Mail{
		From: "help@business.example", To: "customer@example.net", Subject: "Re: question",
		BodyText: "Support text", BodyHTML: "<p>Support text</p>",
		MessageID: "reply@business.example", InReplyTo: "question@customer.example",
		References: []string{"root@customer.example", "question@customer.example"},
		ReplyTo:    "support+case-sensitive-token@inbound.example", AutoSubmitted: "auto-replied",
		ExtraHeaders: map[string]string{"X-Support-Case": "case-42"},
	}
}

func newTransport(t *testing.T, cfg config.Config, deps Dependencies) Transport {
	t.Helper()
	transport, err := New(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	return transport
}

func assertHeaders(t *testing.T, message capturedMail, expected map[string]string) {
	t.Helper()
	for key, want := range expected {
		if got := message.header.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestChosenBackendDeliversSupportAndAccount(t *testing.T) {
	for _, mode := range []string{"smtp", "resend", "ses"} {
		t.Run(mode, func(t *testing.T) {
			cfg, deps, capture := localTransport(t, mode, false)
			var logs bytes.Buffer
			deps.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			checker := &suppressionResult{}
			deps.Suppression = checker
			transport := newTransport(t, cfg, deps)
			message := supportMessage()
			if mode != "smtp" {
				// An unusable tenant key must not sign the configured API identity.
				message.DKIM = &notify.DKIMConfig{Domain: "business.example", Selector: "tenant"}
			}
			if err := transport.Sender.Send(context.Background(), message); err != nil {
				t.Fatalf("send support: %v", err)
			}
			wire := capture.next(t)
			from := "help@business.example"
			if mode != "smtp" {
				from = `"System Accounts" <accounts@system.example>`
			}
			assertHeaders(t, wire, map[string]string{
				"From": from, "To": message.To, "Subject": message.Subject,
				"Message-ID": "<reply@business.example>", "In-Reply-To": "<question@customer.example>",
				"References": "<root@customer.example> <question@customer.example>",
				"Reply-To":   message.ReplyTo, "X-Support-Case": "case-42", "Auto-Submitted": "auto-replied",
			})
			if wire.text != message.BodyText || wire.html != message.BodyHTML {
				t.Errorf("support bodies = %q / %q", wire.text, wire.html)
			}
			if wire.header.Get("DKIM-Signature") != "" {
				t.Error("API mail inherited a tenant DKIM signature")
			}
			if len(message.ExtraHeaders) != 1 {
				t.Error("sending mutated the caller's shared headers")
			}
			account := mailer.Message{To: "new-member@example.net", Subject: "Verify account", Body: "https://example.net/verify?token=" + secretMarker}
			if err := transport.Mailer.Send(context.Background(), account); err != nil {
				t.Fatalf("send account: %v", err)
			}
			wire = capture.next(t)
			assertHeaders(t, wire, map[string]string{
				"From": `"System Accounts" <accounts@system.example>`, "To": account.To, "Subject": account.Subject,
			})
			if wire.text != account.Body || wire.html != "" {
				t.Errorf("account body = %q / %q", wire.text, wire.html)
			}
			if id := wire.header.Get("Message-ID"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@system.example>") {
				t.Errorf("account Message-ID = %q", id)
			}
			if mode == "smtp" && wire.envelope != "<accounts@system.example>" {
				t.Errorf("SMTP envelope = %q", wire.envelope)
			}
			if logs.Len() != 0 {
				t.Fatal("production sending logged message data")
			}
			if got := capture.requests.Load(); got != 2 {
				t.Errorf("provider requests = %d, want only two sends (no verification prerequisites)", got)
			}
			if checker.calls != 2 {
				t.Errorf("successful messages must each check suppression once, got %d", checker.calls)
			}
		})
	}
}

func TestSESOptionalConfigurationSet(t *testing.T) {
	cfg, deps, capture := localTransport(t, "ses", false)
	cfg.OutboundSESConfigurationSet = "transactional-events"
	transport := newTransport(t, cfg, deps)
	if err := transport.Sender.Send(context.Background(), supportMessage()); err != nil {
		t.Fatal(err)
	}
	if got := capture.next(t).configurationSet; got != cfg.OutboundSESConfigurationSet {
		t.Errorf("SES configuration set = %q", got)
	}
}

func TestProviderRejectionNeverFallsBackOrLeaks(t *testing.T) {
	for _, mode := range []string{"smtp", "resend", "ses"} {
		t.Run(mode, func(t *testing.T) {
			cfg, deps, capture := localTransport(t, mode, true)
			fallback, host, port := smtpCapture(t, false)
			if mode != "smtp" {
				cfg.SMTPHost, cfg.SMTPPort = host, port
			}
			var logs bytes.Buffer
			deps.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			transport := newTransport(t, cfg, deps)
			if err := transport.Sender.Send(context.Background(), supportMessage()); !errors.Is(err, notify.ErrNotAccepted) || strings.Contains(err.Error(), secretMarker) {
				t.Fatalf("support rejection = %v", err)
			}
			if err := transport.Mailer.Send(context.Background(), mailer.Message{To: "member@example.net", Body: secretMarker}); !errors.Is(err, mailer.ErrNotAccepted) || strings.Contains(err.Error(), secretMarker) {
				t.Fatalf("account rejection = %v", err)
			}
			if capture.requests.Load() != 2 || fallback.requests.Load() != 0 {
				t.Errorf("requests selected=%d fallback=%d", capture.requests.Load(), fallback.requests.Load())
			}
			if logs.Len() != 0 {
				t.Fatal("production failure logged message or provider data")
			}
		})
	}
}

type suppressionResult struct {
	suppressed bool
	err        error
	calls      int
}

func (s *suppressionResult) IsSuppressed(context.Context, string) (bool, error) {
	s.calls++
	return s.suppressed, s.err
}

func TestSuppressionRefusesBothPortsBeforeNetwork(t *testing.T) {
	for _, mode := range []string{"smtp", "resend", "ses"} {
		for _, failure := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/check-error=%v", mode, failure), func(t *testing.T) {
				cfg, deps, capture := localTransport(t, mode, false)
				checker := &suppressionResult{suppressed: true}
				if failure {
					checker.err = errors.New(secretMarker)
				}
				deps.Suppression = checker
				transport := newTransport(t, cfg, deps)
				err := transport.Sender.Send(context.Background(), supportMessage())
				if err == nil || strings.Contains(err.Error(), secretMarker) || (!failure && !errors.Is(err, notify.ErrSuppressed)) {
					t.Fatalf("support suppression = %v", err)
				}
				err = transport.Mailer.Send(context.Background(), mailer.Message{To: "blocked@example.net", Body: secretMarker})
				if !errors.Is(err, mailer.ErrNotAccepted) || strings.Contains(err.Error(), secretMarker) {
					t.Fatalf("account suppression = %v", err)
				}
				if capture.requests.Load() != 0 || checker.calls != 2 {
					t.Errorf("requests=%d suppression checks=%d", capture.requests.Load(), checker.calls)
				}
			})
		}
	}
}

type suppressedMailbox string

func (mailbox suppressedMailbox) IsSuppressed(_ context.Context, recipient string) (bool, error) {
	return recipient == string(mailbox), nil
}

func TestDisplayNamesCannotBypassMailboxSuppression(t *testing.T) {
	for _, mode := range []string{"smtp", "resend", "ses"} {
		t.Run(mode, func(t *testing.T) {
			cfg, deps, capture := localTransport(t, mode, false)
			deps.Suppression = suppressedMailbox("blocked@example.net")
			transport := newTransport(t, cfg, deps)
			message := supportMessage()
			message.To = "Recipient <blocked@example.net>"
			if err := transport.Sender.Send(context.Background(), message); !errors.Is(err, notify.ErrSuppressed) {
				t.Fatalf("display name bypassed support suppression: %v", err)
			}
			if err := transport.Mailer.Send(context.Background(), mailer.Message{
				To: message.To, Body: secretMarker,
			}); !errors.Is(err, notify.ErrSuppressed) || !errors.Is(err, mailer.ErrNotAccepted) {
				t.Fatalf("display name bypassed transactional suppression: %v", err)
			}
			if capture.requests.Load() != 0 {
				t.Fatal("suppressed mailbox reached a transport")
			}
		})
	}
}

func TestDisabledPrecedesCredentialsAndEveryBackend(t *testing.T) {
	for _, mode := range []string{"smtp", "resend", "ses"} {
		t.Run(mode, func(t *testing.T) {
			cfg, deps, capture := localTransport(t, mode, false)
			cfg.OutboundMailDisabled = true
			cfg.OutboundResendAPIKey, cfg.OutboundSESAccessKeyID, cfg.OutboundSESSecretAccessKey = "", "", ""
			cfg.OutboundSESRegion, cfg.OutboundFromEmail = "", ""
			checker := &suppressionResult{err: errors.New(secretMarker)}
			deps.Suppression = checker
			var logs bytes.Buffer
			deps.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			transport := newTransport(t, cfg, deps)
			if err := transport.Sender.Send(context.Background(), supportMessage()); !errors.Is(err, notify.ErrNotAccepted) {
				t.Fatalf("disabled support = %v", err)
			}
			if err := transport.Mailer.Send(context.Background(), mailer.Message{To: "member@example.net", Body: secretMarker}); !errors.Is(err, mailer.ErrNotAccepted) {
				t.Fatalf("disabled account = %v", err)
			}
			if capture.requests.Load() != 0 || checker.calls != 0 || logs.Len() != 0 {
				t.Fatal("disabled transport performed network, suppression, or logging work")
			}
		})
	}
}

func TestSMTPWithoutSystemIdentityStillSendsSupport(t *testing.T) {
	cfg, deps, capture := localTransport(t, "smtp", false)
	cfg.OutboundFromEmail, cfg.OutboundFromName = "", ""
	transport := newTransport(t, cfg, deps)
	if err := transport.Sender.Send(context.Background(), supportMessage()); err != nil {
		t.Fatal(err)
	}
	assertHeaders(t, capture.next(t), map[string]string{"From": "help@business.example"})
	if err := transport.Mailer.Send(context.Background(), mailer.Message{To: "member@example.net"}); !errors.Is(err, mailer.ErrNotAccepted) {
		t.Fatalf("account without identity = %v", err)
	}
	if capture.requests.Load() != 1 {
		t.Fatal("account mail used an unconfigured identity")
	}
}

func TestMissingSMTPUsesOnlyDevelopmentSink(t *testing.T) {
	for _, environment := range []string{"production", "development"} {
		t.Run(environment, func(t *testing.T) {
			var logs bytes.Buffer
			transport := newTransport(t, config.Config{
				Environment: environment, OutboundProvider: "smtp",
			}, Dependencies{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			message := supportMessage()
			message.BodyText = secretMarker
			if err := transport.Sender.Send(context.Background(), message); !errors.Is(err, notify.ErrNotAccepted) {
				t.Fatalf("unconfigured support = %v", err)
			}
			if strings.Contains(logs.String(), secretMarker) || strings.Contains(logs.String(), message.To) || strings.Contains(logs.String(), message.ReplyTo) {
				t.Fatal("support development metadata leaked message contents")
			}
			logs.Reset()
			err := transport.Mailer.Send(context.Background(), mailer.Message{To: "member@example.net", Body: secretMarker})
			if environment == "production" {
				if !errors.Is(err, mailer.ErrNotAccepted) || logs.Len() != 0 {
					t.Fatalf("production without SMTP = %v; logs = %q", err, logs.String())
				}
			} else if err != nil || !strings.Contains(logs.String(), secretMarker) {
				t.Fatal("development account sink no longer supports local verification flows")
			}
		})
	}
}

func TestHeaderInjectionNeverReachesTransport(t *testing.T) {
	for _, mode := range []string{"smtp", "resend", "ses"} {
		t.Run(mode, func(t *testing.T) {
			cfg, deps, capture := localTransport(t, mode, false)
			transport := newTransport(t, cfg, deps)
			message := supportMessage()
			message.ReplyTo += "\r\nBcc: attacker@example.net"
			if err := transport.Sender.Send(context.Background(), message); !errors.Is(err, notify.ErrNotAccepted) {
				t.Fatalf("injected support header = %v", err)
			}
			err := transport.Mailer.Send(context.Background(), mailer.Message{
				To: "member@example.net", Subject: "verify\r\nBcc: attacker@example.net", Body: secretMarker,
			})
			if !errors.Is(err, mailer.ErrNotAccepted) || capture.requests.Load() != 0 {
				t.Fatalf("injected account header = %v; requests = %d", err, capture.requests.Load())
			}
		})
	}
}
