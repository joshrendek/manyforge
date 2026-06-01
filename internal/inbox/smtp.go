package inbox

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	smtp "github.com/emersion/go-smtp"
	"github.com/jackc/pgx/v5"
)

// maxSMTPRecipients caps recipients per SMTP transaction. Inbound support mail is
// almost always a single RCPT; a small cap bounds fan-out/abuse while tolerating
// the occasional Cc-style multi-delivery. The library enforces this and rejects the
// (cap+1)-th RCPT itself.
const maxSMTPRecipients = 10

// genericRejectMessage is the SINGLE message returned for EVERY rejected recipient
// — unknown address, address we do not handle, and an address that resolves to a
// different tenant are byte-for-byte identical (security: MF-002-INGEST-SCOPE /
// no-oracle, FR-003/SC-006). Never vary it by reason; the difference would be a
// recipient-existence oracle.
const genericRejectMessage = "recipient rejected"

// errGenericReject is the one *smtp.SMTPError used for every RCPT rejection. It is a
// permanent 550 5.1.1; because it is a single shared value, callers cannot tell
// "no such mailbox" from "not handled here" from "not yours".
var errGenericReject = &smtp.SMTPError{
	Code:         550,
	EnhancedCode: smtp.EnhancedCode{5, 1, 1},
	Message:      genericRejectMessage,
}

// errTempFailure is the generic 451 returned when ingestion hits a genuine internal
// error: the sender should retry, and the reply NEVER echoes the wrapped error (it
// is logged server-side instead). 4xx so the message is queued by the sender, not
// bounced — an internal blip must not lose a customer's mail.
var errTempFailure = &smtp.SMTPError{
	Code:         451,
	EnhancedCode: smtp.EnhancedCode{4, 3, 0},
	Message:      "temporary failure, please retry",
}

// errNoRecipient is returned when DATA is issued with no accepted recipient — a
// client sequencing error.
var errNoRecipient = &smtp.SMTPError{
	Code:         554,
	EnhancedCode: smtp.EnhancedCode{5, 5, 1},
	Message:      "no valid recipients",
}

// RecipientValidator decides whether an envelope recipient routes to a business,
// WITHOUT revealing why a miss occurred. *Service satisfies it via CanRoute. The
// session depends on this narrow interface (not the concrete *Service) so the RCPT
// no-oracle behavior is testable with a fake.
type RecipientValidator interface {
	// CanRoute reports whether recipient resolves to exactly one business. A miss
	// (unknown address, unverified custom domain, domain we do not handle) and any
	// internal lookup error both return false — no detail escapes.
	CanRoute(ctx context.Context, recipient string) bool
}

// CanRoute reports whether recipient resolves to a business, run principal-less
// through the resolve_inbound_address SECURITY DEFINER path (the same lookup the
// ingestion tx performs). It swallows the no-route sentinel AND any DB error as a
// plain false: the SMTP RCPT handler must never leak why a recipient was rejected
// (FR-003/SC-006). The lookup is read-only and the tx is rolled back implicitly
// (WithTx commits only on a nil return; we always return nil so the empty tx is a
// harmless no-op commit).
//
// security: MF-002-INGEST-SCOPE — a non-resolving recipient is indistinguishable
// from a resolving one's REJECTION reason at the protocol layer.
func (s *Service) CanRoute(ctx context.Context, recipient string) bool {
	normalized, _ := normalizeRecipient(recipient)
	var routed bool
	err := s.db.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := resolveRecipient(ctx, tx, normalized); err != nil {
			// errNoRoute (no match) OR a real DB error: both collapse to "cannot
			// route" with zero detail. A real DB error is logged for diagnostics but
			// never surfaced to the SMTP peer.
			if !IsNoRoute(err) {
				s.logger.WarnContext(ctx, "inbox: CanRoute lookup failed", "err", err)
			}
			return nil
		}
		routed = true
		return nil
	})
	if err != nil {
		// A tx-level failure (acquire/begin) is also opaque to the peer.
		s.logger.WarnContext(ctx, "inbox: CanRoute tx failed", "err", err)
		return false
	}
	return routed
}

// SMTPAdapter is the in-process inbound SMTP receiver (T029). It is inbound-only and
// never relays: a recipient whose address does not resolve to a business is rejected
// at RCPT with the generic 550, identical for every rejection reason. Accepted DATA
// is handed to the same Ingester the webhook adapter uses, so an SMTP-delivered
// message produces the EXACT same ticket/requester/message shape as a webhook one.
type SMTPAdapter struct {
	server *smtp.Server
}

// NewSMTPAdapter builds the in-process SMTP receiver listening on addr. ing and
// validator are the *Service (injected as interfaces so they can be faked in
// tests). maxBytes caps a single message (MaxMessageBytes); the library rejects an
// oversize message with 552. tlsConfig is OPTIONAL: when non-nil, STARTTLS is
// advertised opportunistically (inbound MX is best-effort TLS — we never require
// it, so dev without a cert still works). logger is used for server-internal errors
// and per-session diagnostics.
func NewSMTPAdapter(addr string, ing Ingester, validator RecipientValidator, maxBytes int64, tlsConfig *tls.Config, logger *slog.Logger) *SMTPAdapter {
	if logger == nil {
		logger = slog.Default()
	}
	be := &smtpBackend{
		ingester:  ing,
		validator: validator,
		maxBytes:  maxBytes,
		logger:    logger,
	}
	srv := smtp.NewServer(be)
	srv.Addr = addr
	srv.MaxMessageBytes = maxBytes
	srv.MaxRecipients = maxSMTPRecipients
	srv.ReadTimeout = 30 * time.Second
	srv.WriteTimeout = 30 * time.Second
	srv.ErrorLog = smtpLogger{logger: logger}
	// Opportunistic STARTTLS: advertise only when a cert is available. We never set
	// AllowInsecureAuth-required TLS; inbound MX must accept plaintext (no MTA is
	// guaranteed to STARTTLS), so a missing cert in dev is fine.
	if tlsConfig != nil {
		srv.TLSConfig = tlsConfig
	}
	return &SMTPAdapter{server: srv}
}

// Provider returns the audit/source attribution key (InboundSource).
func (a *SMTPAdapter) Provider() string { return "smtp" }

// ListenAndServe binds the listener and serves until Close/Shutdown. It returns
// smtp.ErrServerClosed on a clean shutdown (the caller treats that as non-fatal).
func (a *SMTPAdapter) ListenAndServe() error { return a.server.ListenAndServe() }

// Shutdown gracefully stops the receiver, waiting for in-flight sessions to finish
// or ctx to expire.
func (a *SMTPAdapter) Shutdown(ctx context.Context) error { return a.server.Shutdown(ctx) }

// Close immediately closes all listeners and connections.
func (a *SMTPAdapter) Close() error { return a.server.Close() }

// smtpBackend mints one smtpSession per connection. It carries the shared
// dependencies; per-connection state (sender, accepted recipients) lives on the
// session.
type smtpBackend struct {
	ingester  Ingester
	validator RecipientValidator
	maxBytes  int64
	logger    *slog.Logger
}

// NewSession is called once per inbound connection. It captures the peer IP for
// abuse/rate context (never trusted for routing) and a per-connection context.
func (b *smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &smtpSession{
		ingester:  b.ingester,
		validator: b.validator,
		maxBytes:  b.maxBytes,
		logger:    b.logger,
		remoteIP:  peerIP(c),
		ctx:       context.Background(),
	}, nil
}

// smtpSession holds one in-flight SMTP transaction. recipients holds ONLY the
// recipients that passed the CanRoute gate at RCPT time, so DATA never ingests for
// an unrouted address.
type smtpSession struct {
	ingester  Ingester
	validator RecipientValidator
	maxBytes  int64
	logger    *slog.Logger
	remoteIP  string
	ctx       context.Context

	from       string
	recipients []string
}

// Mail records the envelope sender (MAIL FROM). We accept any sender — SPF is
// flagged downstream (FR-019), never used to reject at the protocol layer.
func (s *smtpSession) Mail(from string, _ *smtp.MailOptions) error {
	s.from = from
	return nil
}

// Rcpt validates the recipient against the routing allowlist with NO ORACLE. A
// recipient that resolves to a business is accepted (250); ANY non-resolving
// recipient — unknown address, domain we do not handle (inbound-only, never relay),
// or an address belonging to another tenant — gets the SAME generic 550. The
// rejection reply is identical across all three so the protocol is not an
// existence oracle (FR-003/SC-006, security: MF-002-INGEST-SCOPE).
func (s *smtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if !s.validator.CanRoute(s.ctx, to) {
		return errGenericReject
	}
	s.recipients = append(s.recipients, to)
	return nil
}

// Data reads the (capped) message bytes and hands one RawMessage per accepted
// recipient to the Ingester. Outcome mapping is UNIFORM with the webhook path:
//   - success, duplicate, AND IsNoRoute → 250 accepted (no oracle at DATA either;
//     RCPT already gated, and a race where resolution drops between RCPT and DATA
//     must still look like an accept).
//   - a genuine internal error → 451 temporary failure (generic; the wrapped error
//     is logged server-side and NEVER echoed, so nothing leaks and the sender
//     retries rather than bouncing a real customer's mail).
//
// Multi-RCPT: we ingest ONCE PER accepted recipient. ingest_inbound_message is
// idempotent on (tenant_root_id, message_id), so distinct recipients in the same
// tenant that share a Message-ID dedupe to one ticket_message; recipients in
// different tenants each get their own (correctly scoped) ticket. Reading the body
// once and replaying the bytes keeps each ingest independent and correct.
func (s *smtpSession) Data(r io.Reader) error {
	if len(s.recipients) == 0 {
		return errNoRecipient
	}

	// Read the message under the session cap. The server also enforces
	// MaxMessageBytes at the protocol layer (552), but we re-cap here as
	// defense-in-depth so a future caller of the session cannot bypass it.
	limited := io.LimitReader(r, s.maxBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		s.logger.ErrorContext(s.ctx, "inbox: smtp read DATA failed", "err", err, "remote_ip", s.remoteIP)
		return errTempFailure
	}
	if int64(len(raw)) > s.maxBytes {
		return &smtp.SMTPError{
			Code:         552,
			EnhancedCode: smtp.EnhancedCode{5, 3, 4},
			Message:      "message exceeds size limit",
		}
	}

	now := time.Now()
	for _, rcpt := range s.recipients {
		msg := RawMessage{
			Provider:          "smtp",
			EnvelopeRecipient: rcpt,
			EnvelopeSender:    s.from,
			RemoteIP:          s.remoteIP,
			ReceivedAt:        now,
			Raw:               raw,
		}
		if _, err := s.ingester.Ingest(s.ctx, msg); err != nil {
			if IsNoRoute(err) {
				// Uniform with RCPT/webhook: an unroutable message is a silent accept,
				// never an oracle. Nothing was written.
				continue
			}
			// Genuine internal error: log the WRAPPED error server-side, return a
			// generic 451 so the sender retries. Never echo err.Error().
			s.logger.ErrorContext(s.ctx, "inbox: smtp ingest failed",
				"err", err, "recipient", rcpt, "remote_ip", s.remoteIP)
			return errTempFailure
		}
	}
	return nil
}

// Reset discards the in-flight transaction (sender + accepted recipients) so a
// reused session cannot leak prior-transaction state into the next message.
func (s *smtpSession) Reset() {
	s.from = ""
	s.recipients = nil
}

// Logout frees per-session resources. Nothing to release (no DB handle is held on
// the session; the Service owns the pool).
func (s *smtpSession) Logout() error { return nil }

// peerIP returns the connecting client's IP (host portion of the remote address),
// recorded for abuse/rate context only — it is NOT trusted for routing or auth.
func peerIP(c *smtp.Conn) string {
	if c == nil {
		return ""
	}
	conn := c.Conn()
	if conn == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return conn.RemoteAddr().String()
	}
	return host
}

// smtpLogger adapts slog to the go-smtp Logger interface for server-internal errors.
type smtpLogger struct{ logger *slog.Logger }

func (l smtpLogger) Printf(format string, v ...interface{}) {
	l.logger.Error("smtp: " + fmt.Sprintf(format, v...))
}

func (l smtpLogger) Println(v ...interface{}) {
	l.logger.Error("smtp: " + fmt.Sprintln(v...))
}
