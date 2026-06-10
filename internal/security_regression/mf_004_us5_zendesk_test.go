// mf_004_us5_zendesk (Spec 004 US5 §7, manyforge-a7j.5): pins for the Zendesk connector.
// Source pins are pure string matches (no infrastructure). Behavioral pins exercise the
// real zendesk client + the connector-agnostic Registry, asserting the security-critical
// Zendesk-specific invariants (SSRF dial-refusal, API-token "/token" Basic auth, the
// base64 ts+body webhook HMAC, ticket-id traversal rejection).
//
// Finding IDs (Spec 004 US5 §7):
//   - MF-004-US5-SSRF      — the zendesk factory builds its client via netsafe, never a raw http client.
//   - MF-004-US5-AUTH      — API-token auth uses the Zendesk "<email>/token" Basic username form.
//   - MF-004-US5-WEBHOOK   — webhook verify uses the Zendesk X-Zendesk-Webhook-Signature base64 HMAC and fails closed.
//   - MF-004-US5-TRAVERSAL — the external ticket id is validated (numeric) before any URL build.
//   - MF-004-US5-REUSE     — the agnostic Registry builds a real zendesk connector from a ResolvedConnector.
package security_regression

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/connectors/zendesk"
)

func readZendeskSource(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "connectors", "zendesk", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// MF-004-US5-SSRF — the factory must build its HTTP client through netsafe, with BOTH
// the loopback and private-address gates bound to the per-connector trust flag. We collapse
// whitespace before matching so a future gofmt re-alignment of the Options{} literal (e.g. a
// longer-named field added) does not spuriously fail — the security property, not the column
// alignment, is what we pin. The matched pairs keep field-name and rc.AllowPrivateBaseURL
// adjacent, so a regression to `AllowPrivate: true` would NOT satisfy the pin.
func TestMF004US5_FactoryUsesNetsafe(t *testing.T) {
	norm := strings.Join(strings.Fields(readZendeskSource(t, "factory.go")), " ")
	if !strings.Contains(norm, "netsafe.NewClientWithOptions") {
		t.Fatal("MF-004-US5-SSRF SOURCE PIN: zendesk factory no longer builds its client via netsafe.NewClientWithOptions — SSRF guard bypassed")
	}
	for _, gate := range []string{
		"AllowLoopback: rc.AllowPrivateBaseURL",
		"AllowPrivate: rc.AllowPrivateBaseURL",
	} {
		if !strings.Contains(norm, gate) {
			t.Fatalf("MF-004-US5-SSRF SOURCE PIN: %q gone — private/loopback access no longer gated by the per-connector trust flag", gate)
		}
	}
}

// MF-004-US5-AUTH — Zendesk API-token Basic auth uses the "<email>/token" username form.
func TestMF004US5_TokenBasicAuthForm(t *testing.T) {
	src := readZendeskSource(t, "client.go")
	if !strings.Contains(src, `c.email+"/token"`) {
		t.Fatal(`MF-004-US5-AUTH SOURCE PIN: client.go no longer uses the Zendesk "<email>/token" Basic-auth username form`)
	}
	if !strings.Contains(src, "SetBasicAuth") {
		t.Fatal("MF-004-US5-AUTH SOURCE PIN: client.go no longer authenticates via HTTP Basic")
	}
}

// MF-004-US5-WEBHOOK — webhook verify pins the Zendesk header + base64 HMAC over ts+body.
func TestMF004US5_WebhookSignatureScheme(t *testing.T) {
	src := readZendeskSource(t, "client.go")
	for _, must := range []string{
		"X-Zendesk-Webhook-Signature",
		"X-Zendesk-Webhook-Signature-Timestamp",
		"base64.StdEncoding.DecodeString",
		"hmac.Equal",
	} {
		if !strings.Contains(src, must) {
			t.Fatalf("MF-004-US5-WEBHOOK SOURCE PIN: client.go no longer references %q — webhook verify scheme changed", must)
		}
	}
}

// MF-004-US5-TRAVERSAL — the ticket id is validated (numeric only) before URL builds.
func TestMF004US5_TicketIDValidated(t *testing.T) {
	src := readZendeskSource(t, "client.go")
	if !strings.Contains(src, "`^[0-9]+$`") {
		t.Fatal("MF-004-US5-TRAVERSAL SOURCE PIN: ticketIDRe numeric guard removed — a crafted webhook id could smuggle path traversal")
	}
}

// MF-004-US5-WEBHOOK (behavioral) — forged / empty-secret signatures are rejected.
func TestMF004US5_VerifyWebhookFailsClosed(t *testing.T) {
	conn, err := zendesk.NewFactory(2 * time.Second)(connectors.ResolvedConnector{
		ID: "11111111-1111-1111-1111-111111111111", Type: "zendesk",
		BaseURL:    "https://acme.zendesk.com",
		Credential: connectors.Credential{Email: "ops@acme.test", APIToken: "tok", WebhookSecret: "whsec"},
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	body := []byte(`{"detail":{"id":"12345"}}`)
	hdr := http.Header{
		"X-Zendesk-Webhook-Signature":           []string{"AAAA"},
		"X-Zendesk-Webhook-Signature-Timestamp": []string{"1"},
	}
	if err := conn.VerifyWebhook(hdr, body); !errors.Is(err, zendesk.ErrBadSig) {
		t.Fatalf("MF-004-US5-WEBHOOK: forged sig err = %v, want Is(ErrBadSig)", err)
	}
}

// MF-004-US5-SSRF (behavioral) — the real client refuses to dial a cloud-metadata IP even
// though it is an absolute http URL; netsafe blocks the dial, surfaced as ErrUnreachable.
func TestMF004US5_SSRFDialRefusal(t *testing.T) {
	conn, err := zendesk.NewFactory(2 * time.Second)(connectors.ResolvedConnector{
		ID: "22222222-2222-2222-2222-222222222222", Type: "zendesk",
		BaseURL:             "http://169.254.169.254", // cloud metadata; trust flag false
		AllowPrivateBaseURL: false,
		Credential:          connectors.Credential{Email: "ops@acme.test", APIToken: "tok", WebhookSecret: "whsec"},
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	_, err = conn.FetchIssue(context.Background(), "12345")
	if !errors.Is(err, zendesk.ErrUnreachable) {
		t.Fatalf("MF-004-US5-SSRF: FetchIssue to metadata IP err = %v, want Is(ErrUnreachable) (netsafe dial refusal)", err)
	}
}

// MF-004-US5-REUSE (behavioral) — the connector-agnostic Registry builds a real zendesk
// connector from a ResolvedConnector (proving "thin 2nd reusing US1–US4" wiring works).
func TestMF004US5_RegistryBuildsZendesk(t *testing.T) {
	reg := connectors.NewRegistry(nil) // BuildSystem path does not touch the Service
	reg.Register("zendesk", zendesk.NewFactory(5*time.Second))
	conn, err := reg.BuildSystem(connectors.ResolvedConnector{
		ID: "33333333-3333-3333-3333-333333333333", Type: "zendesk",
		BaseURL:    "https://acme.zendesk.com",
		Credential: connectors.Credential{Email: "ops@acme.test", APIToken: "tok", WebhookSecret: "whsec"},
	})
	if err != nil {
		t.Fatalf("MF-004-US5-REUSE: registry BuildSystem(zendesk) = %v", err)
	}
	if conn == nil {
		t.Fatal("MF-004-US5-REUSE: registry returned a nil zendesk connector")
	}
}
