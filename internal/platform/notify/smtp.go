package notify

import (
	"bytes"
	"context"
	"crypto"
	"fmt"
	"net/smtp"
	"strings"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/manyforge/manyforge/internal/platform/mailer"
)

// DKIMConfig is optional; nil ⇒ unsigned mail (valid for the system domain in dev /
// un-provisioned envs). Set ⇒ outbound is DKIM-signed (FR-013 deliverability).
type DKIMConfig struct {
	Domain     string
	Selector   string
	PrivateKey crypto.Signer // ed25519.PrivateKey or *rsa.PrivateKey
}

// SMTPConfig drives the real sender. Host == "" means "not configured" — callers
// fall back to LogSender.
type SMTPConfig struct {
	Host, Username, Password string
	Port                     int
	DKIM                     *DKIMConfig // optional
}

// SMTPSender implements Sender over a real MTA, with optional DKIM + suppression gate.
type SMTPSender struct {
	cfg         SMTPConfig
	suppression mailer.SuppressionChecker // may be nil
}

// NewSMTPSender creates an SMTPSender wired with the given config and optional
// suppression checker.
func NewSMTPSender(cfg SMTPConfig, suppression mailer.SuppressionChecker) *SMTPSender {
	return &SMTPSender{cfg: cfg, suppression: suppression}
}

// Send dispatches m via the configured SMTP relay. It gates on the suppression
// list before building the MIME payload, and optionally DKIM-signs when a key is
// present.
func (s *SMTPSender) Send(ctx context.Context, m Mail) error {
	if s.suppression != nil {
		suppressed, err := s.suppression.IsSuppressed(ctx, m.To)
		if err != nil {
			return fmt.Errorf("suppression check: %w", err)
		}
		if suppressed {
			return ErrSuppressed
		}
	}
	raw, err := buildMIME(m)
	if err != nil {
		return err
	}
	// Single signing chokepoint: the per-message identity (m.DKIM, selected per
	// reply by SendSubscriber.Handle) takes PRECEDENCE over the static process-wide
	// SystemDKIM. nil m.DKIM falls back to s.cfg.DKIM (system domain) when set; nil
	// both ⇒ unsigned.
	dkimCfg := m.DKIM
	if dkimCfg == nil {
		dkimCfg = s.cfg.DKIM
	}
	if dkimCfg != nil {
		signed, serr := signDKIM(raw, *dkimCfg)
		if serr != nil {
			return fmt.Errorf("dkim sign: %w", serr)
		}
		raw = signed
	}
	from := m.EnvelopeFrom
	if from == "" {
		from = m.From
	}
	var auth smtp.Auth
	if s.cfg.Username != "" {
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	}
	return smtp.SendMail(fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port), auth, from, []string{m.To}, raw)
}

// buildMIME renders an RFC 822 message. Pure (no network) so it is unit-tested.
// Header keys/values are the chokepoint for header injection: US2 subjects derive
// from attacker-controlled inbound mail, so a CR/LF in any header key or value is
// rejected (not stripped) to prevent header smuggling / body splitting.
func buildMIME(m Mail) ([]byte, error) {
	var b bytes.Buffer
	var headerErr error
	h := func(k, v string) {
		if v == "" {
			return
		}
		if strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
			if headerErr == nil {
				headerErr = fmt.Errorf("notify: illegal CR/LF in %q header", k)
			}
			return
		}
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	h("From", m.From)
	h("To", m.To)
	h("Subject", m.Subject)
	if m.MessageID != "" {
		h("Message-ID", "<"+m.MessageID+">")
	}
	if m.InReplyTo != "" {
		h("In-Reply-To", "<"+m.InReplyTo+">")
	}
	if len(m.References) > 0 {
		refs := make([]string, len(m.References))
		for i, r := range m.References {
			refs[i] = "<" + r + ">"
		}
		h("References", strings.Join(refs, " "))
	}
	h("Reply-To", m.ReplyTo)
	h("Auto-Submitted", m.AutoSubmitted)
	for k, v := range m.ExtraHeaders {
		h(k, v)
	}
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	if headerErr != nil {
		return nil, headerErr
	}
	b.WriteString("\r\n")
	b.WriteString(m.BodyText)
	if !strings.HasSuffix(m.BodyText, "\r\n") {
		b.WriteString("\r\n")
	}
	return b.Bytes(), nil
}

// signDKIM signs raw (RFC 822 message bytes) with the given DKIM config.
// Produces a new message with the DKIM-Signature header prepended.
func signDKIM(raw []byte, cfg DKIMConfig) ([]byte, error) {
	opts := &dkim.SignOptions{
		Domain:   cfg.Domain,
		Selector: cfg.Selector,
		Signer:   cfg.PrivateKey,
		Hash:     crypto.SHA256,
	}
	var out bytes.Buffer
	if err := dkim.Sign(&out, bytes.NewReader(raw), opts); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
