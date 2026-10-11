package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResendGetReceivedEmailParsesRawURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || req.URL.Path != "/emails/receiving/em_1" {
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
		if req.URL.Query().Get("html_format") != "cid" {
			t.Fatalf("html_format = %q, want cid (keeps inline images out of the metadata)", req.URL.Query().Get("html_format"))
		}
		if req.Header.Get("Authorization") != "Bearer re_test" {
			t.Fatalf("authorization = %q", req.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"object":"email","id":"em_1","to":["support+Tok@support.example.test"],
			"received_for":["support@support.example.test"],"message_id":"<m-1@customer.test>",
			"html":"<p>hi</p>","text":null,"raw":{"download_url":"https://cdn.example.test/raw/em_1?sig=s","expires_at":"2026-10-10T01:00:00Z"}}`)
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	got, err := r.GetReceivedEmail(context.Background(), "em_1", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "em_1" || got.RawURL != "https://cdn.example.test/raw/em_1?sig=s" || got.MessageID != "<m-1@customer.test>" ||
		len(got.To) != 1 || got.To[0] != "support+Tok@support.example.test" ||
		len(got.ReceivedFor) != 1 || got.ReceivedFor[0] != "support@support.example.test" {
		t.Fatalf("received email = %+v", got)
	}
}

func TestResendGetReceivedEmailClassifiesErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		want error
	}{
		{"gone", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, ErrReceivedEmailGone},
		{"no raw", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"id":"em_1","raw":null}`) }, ErrReceivedRawUnavailable},
		{"huge metadata", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"id":"em_1","html":"`+strings.Repeat("a", 2<<20)+`"}`)
		}, ErrReceivedEmailTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.h)
			defer server.Close()
			r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
			if _, err := r.GetReceivedEmail(context.Background(), "em_1", 1024); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"name":"internal_server_error","message":"upstream secret detail"}`)
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	_, err := r.GetReceivedEmail(context.Background(), "em_1", 1024)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadGateway ||
		errors.Is(err, ErrReceivedEmailGone) || strings.Contains(err.Error(), "upstream secret detail") {
		t.Fatalf("5xx error = %v, want a retryable HTTPError without the upstream body", err)
	}
}

func TestResendDownloadReceivedRawCapsAndNeverSendsAPIKey(t *testing.T) {
	raw := []byte("From: a@b.test\r\nTo: s@x.test\r\nSubject: hi\r\n\r\nbody\r\n")
	var chunked atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "" {
			t.Fatalf("raw download sent Authorization %q; the URL is pre-signed", req.Header.Get("Authorization"))
		}
		if chunked.Load() {
			// No Content-Length: the cap must hold while streaming.
			_, _ = w.Write(raw[:10])
			w.(http.Flusher).Flush()
			_, _ = w.Write(raw[10:])
			return
		}
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", Client: server.Client(), AllowInsecureRawURL: true}
	got, err := r.DownloadReceivedRaw(context.Background(), server.URL+"/raw/em_1", int64(len(raw)))
	if err != nil || string(got) != string(raw) {
		t.Fatalf("download = %q, %v", got, err)
	}
	if _, err := r.DownloadReceivedRaw(context.Background(), server.URL+"/raw/em_1", int64(len(raw)-1)); !errors.Is(err, ErrReceivedEmailTooLarge) {
		t.Fatalf("over-cap error = %v, want ErrReceivedEmailTooLarge", err)
	}
	chunked.Store(true)
	if _, err := r.DownloadReceivedRaw(context.Background(), server.URL+"/raw/em_1", int64(len(raw)-1)); !errors.Is(err, ErrReceivedEmailTooLarge) {
		t.Fatalf("chunked over-cap error = %v, want ErrReceivedEmailTooLarge", err)
	}
}

func TestResendDownloadReceivedRawRejectsNonHTTPS(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	r := &Resend{Client: server.Client()}
	for _, u := range []string{server.URL + "/raw", "file:///etc/passwd", "://bad", ""} {
		if _, err := r.DownloadReceivedRaw(context.Background(), u, 1024); !errors.Is(err, ErrReceivedRawUnavailable) {
			t.Fatalf("url %q error = %v, want ErrReceivedRawUnavailable", u, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("non-https URLs reached the network %d times", hits.Load())
	}
}

func TestResendDownloadTransportErrorDoesNotLeakSignedURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := server.URL
	server.Close() // connection refused from here on
	r := &Resend{AllowInsecureRawURL: true}
	_, err := r.DownloadReceivedRaw(context.Background(), base+"/raw?X-Amz-Signature=topsecret", 1024)
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("transport error = %v, must not contain the signed URL", err)
	}
}

func TestResendEnsureWebhookUpgradesLegacyEventsWithPatch(t *testing.T) {
	const endpoint = "https://hub.example.test/api/v1/inbound/mailing/profile-a/resend"
	events := []string{"email.bounced", "email.complained"}
	var patchBody struct {
		Events []string `json:"events"`
	}
	var mutations, patches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks":
			_ = json.NewEncoder(w).Encode(map[string]any{"has_more": false, "data": []any{
				map[string]any{"id": "wh_legacy", "status": "enabled", "endpoint": endpoint, "events": events},
			}})
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks/wh_legacy":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "wh_legacy", "status": "enabled", "endpoint": endpoint,
				"events": events, "signing_secret": "whsec_MDEyMzQ1Njc4OWFiY2RlZg=="})
		case req.Method == http.MethodPatch && req.URL.Path == "/webhooks/wh_legacy":
			patches++
			if err := json.NewDecoder(req.Body).Decode(&patchBody); err != nil {
				t.Fatal(err)
			}
			events = patchBody.Events
			_, _ = io.WriteString(w, `{"object":"webhook","id":"wh_legacy"}`)
		default:
			t.Fatalf("request = %s %s (an upgrade must not delete or recreate)", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	got, created, err := r.EnsureWebhook(context.Background(), endpoint, "wh_legacy", func(context.Context) error {
		mutations++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created || got.ID != "wh_legacy" || got.SigningSecret != "whsec_MDEyMzQ1Njc4OWFiY2RlZg==" {
		t.Fatalf("webhook = %+v created=%v, want the same webhook and secret", got, created)
	}
	if patches != 1 || mutations != 1 || !containsResendEvents(patchBody.Events, ResendWebhookEvents...) {
		t.Fatalf("patches=%d mutations=%d events=%v", patches, mutations, patchBody.Events)
	}
}

func TestResendEnsureReceivingEvents(t *testing.T) {
	events := []string{"email.bounced", "email.complained", "email.delivered"}
	var patches int
	var patched []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/webhooks/wh_1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "wh_1", "status": "enabled",
				"events": events, "signing_secret": "whsec_MDEyMzQ1Njc4OWFiY2RlZg=="})
		case req.Method == http.MethodPatch && req.URL.Path == "/webhooks/wh_1":
			patches++
			var body struct {
				Events []string `json:"events"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			patched, events = body.Events, body.Events
			_, _ = io.WriteString(w, `{"object":"webhook","id":"wh_1"}`)
		default:
			t.Fatalf("request = %s %s", req.Method, req.URL.Path)
		}
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	secret, didPatch, err := r.EnsureReceivingEvents(context.Background(), "wh_1")
	if err != nil || !didPatch || secret != "whsec_MDEyMzQ1Njc4OWFiY2RlZg==" {
		t.Fatalf("first = %q %v %v", secret, didPatch, err)
	}
	if !containsResendEvents(patched, "email.bounced", "email.complained", "email.delivered", "email.received") {
		t.Fatalf("patched events = %v, want the union (existing events kept)", patched)
	}
	if _, didPatch, err = r.EnsureReceivingEvents(context.Background(), "wh_1"); err != nil || didPatch || patches != 1 {
		t.Fatalf("second call patched=%v patches=%d err=%v, want read-only", didPatch, patches, err)
	}
}

func TestResendGetReceivedEmailOversizedErrorResponseIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, strings.Repeat("a", 4<<20))
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", BaseURL: server.URL, Client: server.Client()}
	_, err := r.GetReceivedEmail(context.Background(), "em_1", 16)
	var httpErr *HTTPError
	if errors.Is(err, ErrReceivedEmailTooLarge) || !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("error = %v, want a retryable 503 HTTPError, not the too-large sentinel", err)
	}
}

func TestResendDownloadRejectsHTTPSDowngradeRedirect(t *testing.T) {
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { plainHits.Add(1) }))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, plain.URL+"/raw?sig=topsecret", http.StatusFound)
	}))
	defer tlsSrv.Close()
	r := &Resend{Client: tlsSrv.Client()}
	_, err := r.DownloadReceivedRaw(context.Background(), tlsSrv.URL+"/raw?sig=topsecret", 1024)
	if !errors.Is(err, ErrReceivedRawUnavailable) || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("error = %v, want ErrReceivedRawUnavailable without the signed URL", err)
	}
	if plainHits.Load() != 0 {
		t.Fatalf("downgrade target was hit %d times", plainHits.Load())
	}
}

func TestResendDownloadCapsRedirectHops(t *testing.T) {
	var hits atomic.Int32
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		http.Redirect(w, req, "/raw?sig=topsecret", http.StatusFound)
	}))
	defer tlsSrv.Close()
	r := &Resend{Client: tlsSrv.Client()}
	_, err := r.DownloadReceivedRaw(context.Background(), tlsSrv.URL+"/raw?sig=topsecret", 1024)
	if !errors.Is(err, ErrReceivedRawUnavailable) || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("error = %v, want ErrReceivedRawUnavailable", err)
	}
	if hits.Load() > maxRawRedirects+1 {
		t.Fatalf("followed %d requests, want at most %d", hits.Load(), maxRawRedirects+1)
	}
}

func TestResendDownloadMalformedLocationDoesNotLeakSignedURL(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://x.test/%zz?sig=topsecret")
		w.WriteHeader(http.StatusFound)
	}))
	defer tlsSrv.Close()
	r := &Resend{Client: tlsSrv.Client()}
	_, err := r.DownloadReceivedRaw(context.Background(), tlsSrv.URL+"/raw?sig=topsecret", 1024)
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("error = %v, must not contain the signed URL", err)
	}
}
