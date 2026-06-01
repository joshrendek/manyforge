package inbox

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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

// verify checks the X-MF-Signature against an HMAC-SHA256 over the signed content,
// in constant time. When X-MF-Timestamp is present it is bound into the signed
// content ("<timestamp>.<body>") for replay defense; otherwise the body alone is
// signed. The provided signature is accepted as bare lowercase hex or with a
// "sha256=" prefix (some providers prefix it). An empty secret rejects everything
// (fail closed). Returns true only on a valid signature.
func (a *WebhookAdapter) verify(sig, timestamp string, body []byte) bool {
	if len(a.secret) == 0 || sig == "" {
		return false
	}
	sig = strings.TrimPrefix(strings.TrimSpace(sig), "sha256=")
	provided, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, a.secret)
	if timestamp != "" {
		mac.Write([]byte(timestamp))
		mac.Write([]byte("."))
	}
	mac.Write(body)
	expected := mac.Sum(nil)
	// Constant-time compare so a forged signature cannot be brute-forced byte by
	// byte via response timing. security: MF-002-WEBHOOK-SIG.
	return subtle.ConstantTimeCompare(provided, expected) == 1
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

// remoteIP returns the connecting client's IP (host portion of RemoteAddr). It is
// recorded for abuse/rate context only; it is NOT trusted for routing or auth.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
