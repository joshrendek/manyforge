package inbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Ingester is the boundary the webhook handler depends on: it turns a RawMessage
// into an IngestResult. *Service satisfies it. Depending on the interface (not the
// concrete *Service) lets the handler-level contract be tested without a database.
type Ingester interface {
	Ingest(ctx context.Context, msg RawMessage) (IngestResult, error)
}

// providerDecoder turns a verified request body into a RawMessage for one provider
// adapter key. The provider name and remote IP are passed in by the handler; the
// decoder fills the envelope routing fields and the rfc822 Raw bytes.
type providerDecoder func(provider, remoteIP string, body []byte) (RawMessage, error)

// WebhookAdapter authenticates and decodes inbound provider webhooks. Every
// supported provider currently shares the same generic decoder (the normalized
// InboundEmailEnvelope JSON, plus a message/rfc822 raw body) — the registry exists
// so a provider-specific decoder can be slotted in later WITHOUT touching the
// handler. Authentication is a per-provider HMAC-SHA256 signature verified in
// constant time against the shared secret; an unknown/absent/forged signature is a
// 401 and the ingestion boundary is never reached.
type WebhookAdapter struct {
	secret   []byte
	decoders map[string]providerDecoder
}

// NewWebhookAdapter builds the adapter with the per-provider HMAC secret and the
// generic provider registry. The enum of accepted provider keys is enforced at the
// HTTP layer (chi path param); every key maps to the generic decoder here.
func NewWebhookAdapter(secret string) *WebhookAdapter {
	return &WebhookAdapter{
		secret:   []byte(secret),
		decoders: defaultDecoders(),
	}
}

// defaultDecoders returns the provider registry. Only the generic envelope/rfc822
// decoder is wired (YAGNI: no speculative provider-specific parsers). The OpenAPI
// provider enum is [postmark, mailgun, sendgrid, ses, smtp]; the quickstart also
// uses a literal "webhook" segment. All route through the same generic decoder.
//
// TODO(US4/per-provider): the generic raw-MIME path extracts a single envelope
// recipient best-effort from the message's To/Cc headers (see decodeGeneric). For
// providers that supply authoritative envelope/recipient metadata out-of-band
// (e.g. SES `receipt.recipients`, mailgun `recipient`, postmark `OriginalRecipient`),
// slot in a provider-specific decoder here that reads that metadata instead — it is
// the spoof-resistant routing signal and supports multi-recipient delivery.
func defaultDecoders() map[string]providerDecoder {
	generic := decodeGeneric
	return map[string]providerDecoder{
		"postmark": generic,
		"mailgun":  generic,
		"sendgrid": generic,
		"ses":      generic,
		"smtp":     generic,
		"webhook":  generic, // quickstart literal provider segment
	}
}

// verify checks the X-MF-Signature against the per-provider secret. It delegates to
// the package-shared verifyHMAC (the single crypto source of truth, so this and the
// bounce webhook can never drift apart in auth strength); the thin method survives to
// carry the adapter's secret field and this finding's tag. security: MF-002-WEBHOOK-SIG.
func (a *WebhookAdapter) verify(sig, timestamp string, body []byte) bool {
	return verifyHMAC(a.secret, sig, timestamp, body)
}

// decode resolves the provider's decoder and turns the (already signature-verified)
// request into a RawMessage. An unknown provider or a malformed body is a client
// error.
func (a *WebhookAdapter) decode(r *http.Request, body []byte) (RawMessage, error) {
	provider := chiProvider(r)
	dec, ok := a.decoders[provider]
	if !ok {
		return RawMessage{}, fmt.Errorf("inbox: unknown webhook provider %q", provider)
	}
	return dec(provider, remoteIP(r), body)
}

// decodeGeneric maps the request body to a RawMessage. Two content types are
// supported per the OpenAPI: the normalized InboundEmailEnvelope JSON (primary) and
// raw message/rfc822 bytes (SMTP-style providers). The JSON envelope carries the
// rfc822 bytes in raw_mime (base64) when the provider supplies them; otherwise a
// minimal rfc822 view is synthesized from the structured fields so Parse has bytes
// to work with. Routing always comes from the envelope recipient, never headers.
func decodeGeneric(provider, remoteIP string, body []byte) (RawMessage, error) {
	msg := RawMessage{
		Provider: "webhook:" + provider,
		RemoteIP: remoteIP,
	}

	// A bare rfc822 body (no JSON object) is treated as raw MIME: the body itself is
	// the message and routing/sender are recovered from its headers by Parse. We
	// detect JSON by the leading byte after trimming whitespace.
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		msg.Raw = body
		// The JSON-envelope path carries the recipient explicitly; a bare rfc822
		// body does not, so Service.Ingest (which routes ONLY on EnvelopeRecipient)
		// would no-route and silently drop a delivery the OpenAPI advertises as
		// supported (message/rfc822). Populate EnvelopeRecipient best-effort from the
		// recipient headers: first To address, falling back to the first Cc.
		// ParsedEmail.Recipients is the union of header To then Cc, in order, so
		// Recipients[0] is exactly that (To-first, Cc-fallback). This is a deliberate
		// SINGLE-recipient extraction; multi-recipient / Cc-only / provider-specific
		// routing (e.g. SES receipt.recipients) is a future per-provider-decoder
		// enhancement — see the TODO in defaultDecoders.
		//
		// security: MF-002-INGEST-SCOPE (no-oracle) is preserved — this only chooses a
		// routing CANDIDATE from the header. resolve_inbound_address still gates on the
		// business's actual inbound address existing; an unknown/unresolving recipient
		// (or one we couldn't extract) still yields the uniform 202, disclosing nothing.
		if parsed, err := Parse(body); err == nil || parsed != nil {
			if len(parsed.Recipients) > 0 {
				msg.EnvelopeRecipient = parsed.Recipients[0]
			}
			if parsed.From.Address != "" {
				msg.EnvelopeSender = parsed.From.Address
			}
		}
		return msg, nil
	}

	var env inboundEmailEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return RawMessage{}, fmt.Errorf("inbox: decode envelope: %w", err)
	}

	msg.EnvelopeSender = env.From
	if len(env.To) > 0 {
		msg.EnvelopeRecipient = env.To[0]
	}

	if env.RawMIME != nil && *env.RawMIME != "" {
		raw, err := base64.StdEncoding.DecodeString(*env.RawMIME)
		if err != nil {
			return RawMessage{}, fmt.Errorf("inbox: decode raw_mime: %w", err)
		}
		msg.Raw = raw
		return msg, nil
	}

	msg.Raw = synthesizeRFC822(env)
	return msg, nil
}

// inboundEmailEnvelope mirrors the OpenAPI InboundEmailEnvelope schema. Fields are
// best-effort: a provider may omit any of them.
type inboundEmailEnvelope struct {
	To         []string          `json:"to"`
	From       string            `json:"from"`
	Subject    string            `json:"subject"`
	MessageID  string            `json:"message_id"`
	InReplyTo  *string           `json:"in_reply_to"`
	References []string          `json:"references"`
	BodyText   *string           `json:"body_text"`
	BodyHTML   *string           `json:"body_html"`
	Headers    map[string]string `json:"headers"`
	RawMIME    *string           `json:"raw_mime"`
}

// synthesizeRFC822 builds a minimal rfc822 message from the structured envelope
// fields so the shared Parse path (enmime) can extract subject, threading headers,
// sender, and bodies uniformly — even when the provider sent only JSON. The
// envelope To/From remain the routing source of truth; these headers are for the
// parsed view only.
func synthesizeRFC822(env inboundEmailEnvelope) []byte {
	var b strings.Builder
	writeHeader := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	writeHeader("From", env.From)
	if len(env.To) > 0 {
		writeHeader("To", strings.Join(env.To, ", "))
	}
	writeHeader("Subject", env.Subject)
	if env.MessageID != "" {
		writeHeader("Message-ID", "<"+strings.Trim(env.MessageID, "<>")+">")
	}
	if env.InReplyTo != nil && *env.InReplyTo != "" {
		writeHeader("In-Reply-To", "<"+strings.Trim(*env.InReplyTo, "<>")+">")
	}
	if len(env.References) > 0 {
		refs := make([]string, 0, len(env.References))
		for _, ref := range env.References {
			refs = append(refs, "<"+strings.Trim(ref, "<>")+">")
		}
		writeHeader("References", strings.Join(refs, " "))
	}
	// Preserve auth/loop-guard headers the provider passed through (FR-018/FR-019).
	for k, v := range env.Headers {
		switch http.CanonicalHeaderKey(k) {
		case "Authentication-Results", "Auto-Submitted", "Precedence",
			"X-Auto-Response-Suppress", "List-Id":
			writeHeader(http.CanonicalHeaderKey(k), v)
		}
	}
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", "text/plain; charset=utf-8")
	b.WriteString("\r\n")
	if env.BodyText != nil {
		b.WriteString(*env.BodyText)
	}
	return []byte(b.String())
}

// IPRateLimitKey is the SINGLE per-IP ingest rate-limit key shared by BOTH inbound
// transports (webhook + SMTP). It is the bare client IP with no transport prefix, so
// the same source IP maps to the SAME bucket in the shared ingestIPLimiter whether
// it arrives over HTTP or SMTP — a source cannot evade the per-IP cap by hopping
// transports. The webhook side derives the IP via ratelimit.ClientIP (trusted-proxy
// aware) and the SMTP side from the connection peer; both feed their bare-IP string
// through this function so the key shape can never silently drift apart.
func IPRateLimitKey(ip string) string { return ip }

// remoteIP returns the connecting client's IP (host portion of r.RemoteAddr ONLY).
// X-Forwarded-For is INTENTIONALLY ignored here: this value is recorded for
// abuse/diagnostic context and is NOT trusted for routing or auth. The per-IP rate
// limit that actually gates abuse is keyed via the trusted-CIDR-aware
// ratelimit.ClientIP (wired as middleware in main.go), so a spoofed X-Forwarded-For
// cannot evade the limiter — and must not silently influence this recorded value.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
