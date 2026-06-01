package inbox

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/jhillyerd/enmime/v2"
)

// InboundSource is the marker abstraction every inbound adapter satisfies: the
// provider webhook adapter (T028) and the in-process SMTP receiver (T029). An
// adapter's only job is to turn a provider delivery into a [RawMessage] (filling
// the envelope routing fields and the rfc822 bytes) and hand it to the ingestion
// service — it does not itself parse MIME. The service then calls [Parse] on the
// RawMessage's Raw bytes. The interface is deliberately minimal (YAGNI): there is
// no pull-style polling API, because both adapters are push-driven (HTTP request,
// SMTP session). Provider returns the adapter key (e.g. "webhook:postmark",
// "smtp") used in audit/source attribution and per-recipient metrics.
type InboundSource interface {
	Provider() string
}

// RawMessage is one inbound delivery as the transport handed it to us, before any
// MIME parsing. The envelope fields come from the SMTP/webhook transaction (NOT
// the message headers) and are the source of truth for routing and SPF — header
// To/From can be spoofed or differ from the real recipient/return-path.
type RawMessage struct {
	// Provider is the adapter key that produced this message, e.g.
	// "webhook:postmark" or "smtp". Mirrors InboundSource.Provider and is used as
	// the audit/source attribution string (passed to ingest_inbound_message as
	// p_source).
	Provider string

	// EnvelopeRecipient is the RCPT TO address from the transport — the routing
	// key resolved to exactly one business (T024). May differ from the header To
	// (Bcc, list expansion, plus-addressing), so the envelope value, not the
	// header, drives routing.
	EnvelopeRecipient string

	// EnvelopeSender is the MAIL FROM / return-path from the transport, used as
	// the SPF identity (FR-019). May differ from the header From.
	EnvelopeSender string

	// RemoteIP is the connecting client's IP (SMTP peer or webhook source),
	// recorded for abuse/rate context.
	RemoteIP string

	// ReceivedAt is when the transport accepted the message.
	ReceivedAt time.Time

	// Raw is the rfc822 message bytes. A provider that delivers a structured JSON
	// envelope instead is normalized to rfc822 (or to the parsed fields directly)
	// by its adapter in T028; this slice always holds the MIME bytes that [Parse]
	// consumes.
	Raw []byte
}

// Address is a parsed sender/recipient: the bare email plus an optional display
// name. Address is "" when the header was absent or unparseable.
type Address struct {
	Address string
	Name    string
}

// ParsedEmail is the structured view of a RawMessage's MIME content. It carries
// exactly what the downstream ingestion pipeline needs: threading headers (T025),
// sender/recipients, bodies, attachments, auth results (FR-019), and the loop
// guard (FR-018). Fields are best-effort: any one may be empty on malformed mail.
type ParsedEmail struct {
	// From is the header From address + display name (the requester identity,
	// T026). For SPF use RawMessage.EnvelopeSender instead.
	From Address

	// Recipients is the union of header To and Cc addresses (bare email form).
	// Routing uses RawMessage.EnvelopeRecipient, not this; Recipients is retained
	// for display/diagnostics only.
	Recipients []string

	Subject string

	// MessageID is the RFC822 Message-ID with surrounding angle brackets removed,
	// or "" when absent. We do NOT synthesize one here — threading (T025) decides
	// how to handle a missing id.
	MessageID string

	// InReplyTo is the RFC822 In-Reply-To id (brackets stripped), or "".
	InReplyTo string

	// References is the RFC822 References chain (each id, brackets stripped), in
	// header order. Empty when absent.
	References []string

	// Date is the parsed Date header; zero when absent or unparseable.
	Date time.Time

	TextBody string
	HTMLBody string

	Attachments []ParsedAttachment

	// Auth holds the best-effort SPF/DKIM/DMARC verdicts parsed from
	// Authentication-Results (FR-019: flagged, never used to reject).
	Auth AuthResults

	// Auto holds the auto-reply loop-guard signals (FR-018).
	Auto AutoHeaders
}

// ParsedAttachment is one decoded attachment part. DeclaredContentType is the
// content type from the part header and is UNTRUSTED — the storage layer (SL-E
// blob.Sniff) MIME-sniffs Content's first bytes and ignores this value (FR-007).
type ParsedAttachment struct {
	FileName            string
	DeclaredContentType string
	Content             []byte
}

// AuthResults is the best-effort SPF/DKIM/DMARC verdict triple extracted from the
// first Authentication-Results header. Each field is the lowercase mechanism
// result (e.g. "pass", "fail", "none", "softfail") or "" when the mechanism was
// not reported. These are recorded for display and flagging only — inbound mail
// is never rejected on them (FR-019).
type AuthResults struct {
	SPF   string
	DKIM  string
	DMARC string
}

// AutoHeaders carries the raw auto-response signals plus the derived verdict the
// loop guard consumes (FR-018). IsAutoReply is true when any signal indicates the
// message was machine-generated, so the desk will not auto-respond and risk a
// mail loop.
type AutoHeaders struct {
	// AutoSubmitted is the raw Auto-Submitted header value (e.g. "auto-replied").
	AutoSubmitted string
	// Precedence is the raw Precedence header value (e.g. "bulk", "list").
	Precedence string
	// AutoResponseSuppress is the raw X-Auto-Response-Suppress header value.
	AutoResponseSuppress string
	// ListID is the raw List-Id header value; presence alone marks list mail.
	ListID string

	// IsAutoReply is the derived loop-guard verdict (see deriveIsAutoReply).
	IsAutoReply bool
}

// Parse turns rfc822 bytes into a best-effort *ParsedEmail using
// enmime.ReadEnvelope. It degrades safely: malformed or garbage input never
// panics and always yields a non-nil result (with whatever fields could be
// recovered). A returned error wraps a hard parse failure for logging; callers
// that ingest should still treat the returned *ParsedEmail as usable. We never
// synthesize a Message-ID (that is threading's concern, T025).
func Parse(raw []byte) (parsed *ParsedEmail, err error) {
	// enmime is robust, but defend against a panic on adversarial input so a
	// single bad message can never take down the ingestion path.
	defer func() {
		if r := recover(); r != nil {
			if parsed == nil {
				parsed = &ParsedEmail{}
			}
			err = fmt.Errorf("inbox: panic parsing message: %v", r)
		}
	}()

	env, readErr := enmime.ReadEnvelope(bytes.NewReader(raw))
	if env == nil {
		// Hard failure with no envelope to recover from — return an empty,
		// non-nil result so the caller can proceed (and surface the error).
		return &ParsedEmail{}, fmt.Errorf("inbox: read envelope: %w", readErr)
	}

	pe := &ParsedEmail{
		Subject:    env.GetHeader("Subject"),
		MessageID:  stripAngles(env.GetHeader("Message-ID")),
		InReplyTo:  stripAngles(env.GetHeader("In-Reply-To")),
		References: parseReferences(env.GetHeader("References")),
		TextBody:   env.Text,
		HTMLBody:   env.HTML,
	}

	pe.From = firstAddress(env, "From")
	pe.Recipients = addresses(env, "To", "Cc")

	if d, derr := env.Date(); derr == nil {
		pe.Date = d
	}

	pe.Attachments = collectAttachments(env)
	pe.Auth = parseAuthResults(env.GetHeader("Authentication-Results"))
	pe.Auto = parseAutoHeaders(env)

	// enmime accumulates non-fatal parse warnings in env.Errors; the structured
	// view above is still usable, so we surface readErr (if any) but do not fail.
	if readErr != nil {
		return pe, fmt.Errorf("inbox: read envelope: %w", readErr)
	}
	return pe, nil
}

// firstAddress returns the first address parsed from the named header, or a zero
// Address when the header is absent or unparseable.
func firstAddress(env *enmime.Envelope, header string) Address {
	list, err := env.AddressList(header)
	if err != nil || len(list) == 0 {
		return Address{}
	}
	return Address{Address: list[0].Address, Name: list[0].Name}
}

// addresses returns the bare email addresses across the named headers, in order,
// skipping any that fail to parse.
func addresses(env *enmime.Envelope, headers ...string) []string {
	var out []string
	for _, h := range headers {
		list, err := env.AddressList(h)
		if err != nil {
			continue
		}
		for _, a := range list {
			if a.Address != "" {
				out = append(out, a.Address)
			}
		}
	}
	return out
}

// collectAttachments returns the decoded attachment parts. Inline parts (e.g.
// embedded images) are not treated as user attachments here.
func collectAttachments(env *enmime.Envelope) []ParsedAttachment {
	if len(env.Attachments) == 0 {
		return nil
	}
	out := make([]ParsedAttachment, 0, len(env.Attachments))
	for _, p := range env.Attachments {
		if p == nil {
			continue
		}
		out = append(out, ParsedAttachment{
			FileName:            p.FileName,
			DeclaredContentType: p.ContentType,
			Content:             p.Content,
		})
	}
	return out
}

// stripAngles trims surrounding whitespace and a single pair of angle brackets
// from a header value, yielding the bare id ("<x@y>" -> "x@y").
func stripAngles(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	return strings.TrimSpace(s)
}

// parseReferences splits a References header into its constituent message ids,
// stripping angle brackets from each. Returns nil when the header is empty.
func parseReferences(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if id := stripAngles(f); id != "" {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseAuthResults does a best-effort parse of an Authentication-Results header
// (RFC 8601) into the SPF/DKIM/DMARC verdicts. It is intentionally lenient: the
// values are only flagged, never used to reject (FR-019). Format is roughly
// "authserv-id; method=result key=value; method=result ...".
func parseAuthResults(header string) AuthResults {
	var ar AuthResults
	if strings.TrimSpace(header) == "" {
		return ar
	}
	// Split on ';' into the authserv-id and each method clause; within a clause
	// the verdict is the first whitespace-separated token "method=result".
	for _, clause := range strings.Split(header, ";") {
		fields := strings.Fields(clause)
		if len(fields) == 0 {
			continue
		}
		method, result, ok := strings.Cut(fields[0], "=")
		if !ok {
			continue
		}
		method = strings.ToLower(strings.TrimSpace(method))
		result = strings.ToLower(strings.TrimSpace(result))
		switch method {
		case "spf":
			if ar.SPF == "" {
				ar.SPF = result
			}
		case "dkim":
			if ar.DKIM == "" {
				ar.DKIM = result
			}
		case "dmarc":
			if ar.DMARC == "" {
				ar.DMARC = result
			}
		}
	}
	return ar
}

// parseAutoHeaders extracts the auto-response signal headers and derives the
// loop-guard verdict (FR-018).
func parseAutoHeaders(env *enmime.Envelope) AutoHeaders {
	a := AutoHeaders{
		AutoSubmitted:        strings.TrimSpace(env.GetHeader("Auto-Submitted")),
		Precedence:           strings.TrimSpace(env.GetHeader("Precedence")),
		AutoResponseSuppress: strings.TrimSpace(env.GetHeader("X-Auto-Response-Suppress")),
		ListID:               strings.TrimSpace(env.GetHeader("List-Id")),
	}
	a.IsAutoReply = deriveIsAutoReply(a)
	return a
}

// deriveIsAutoReply implements the FR-018 loop guard. A message is treated as an
// auto-reply when any of these holds:
//   - Auto-Submitted is present and not "no" (RFC 3834: any value other than
//     "no" means machine-generated);
//   - Precedence is bulk/list/junk (mailing-list / bulk mail);
//   - X-Auto-Response-Suppress is present (Microsoft auto-reply marker);
//   - List-Id is present (the message came from a mailing list).
func deriveIsAutoReply(a AutoHeaders) bool {
	if v := strings.ToLower(a.AutoSubmitted); v != "" && v != "no" {
		return true
	}
	switch strings.ToLower(a.Precedence) {
	case "bulk", "list", "junk":
		return true
	}
	if a.AutoResponseSuppress != "" {
		return true
	}
	if a.ListID != "" {
		return true
	}
	return false
}
