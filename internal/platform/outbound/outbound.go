// Package outbound selects the process-wide support and transactional mail transport.
package outbound

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/mail"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/mailing/provider"
	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/platform/mailer"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

// Transport exposes the same selected backend through both existing mail ports.
type Transport struct {
	Sender notify.Sender
	Mailer mailer.Mailer
}

// Dependencies supplies shared services. Endpoint overrides are for local tests only.
type Dependencies struct {
	Logger              *slog.Logger
	Suppression         mailer.SuppressionChecker
	DKIM                *notify.DKIMConfig
	HTTPClient          *http.Client
	ResendBaseURL       string
	SESEndpointResolver sesv2.EndpointResolverV2
}

// New constructs a single backend without sending or verifying campaign resources.
// Tenant mailing profiles remain independent of this process-wide identity.
func New(ctx context.Context, cfg config.Config, deps Dependencies) (Transport, error) {
	if err := cfg.ValidateOutbound(); err != nil {
		return Transport{}, err
	}
	transport := Transport{Sender: notify.DisabledSender{}, Mailer: mailer.DisabledMailer{}}
	if cfg.OutboundMailDisabled {
		return transport, nil
	}
	from := (&mail.Address{Name: cfg.OutboundFromName, Address: cfg.OutboundFromEmail}).String()
	switch cfg.OutboundProvider {
	case "", "smtp":
		if strings.TrimSpace(cfg.SMTPHost) == "" {
			if cfg.Environment == "development" {
				transport.Sender = notify.LogSender{Logger: deps.Logger, Suppression: deps.Suppression}
				transport.Mailer = developmentMailer{sink: mailer.LogMailer{Logger: deps.Logger}, suppression: deps.Suppression}
			}
			return transport, nil
		}
		var dkim *notify.DKIMConfig
		if deps.DKIM != nil {
			copy := *deps.DKIM
			dkim = &copy
		}
		transport.Sender = safeSender{sender: notify.NewSMTPSender(notify.SMTPConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUser, Password: cfg.SMTPPass, DKIM: dkim,
		}, deps.Suppression)}
	case "resend":
		transport.Sender = apiSender{
			delivery: &provider.Resend{APIKey: cfg.OutboundResendAPIKey, FromEmail: cfg.OutboundFromEmail, BaseURL: deps.ResendBaseURL, Client: deps.HTTPClient},
			from:     from, suppression: deps.Suppression, structured: true,
		}
	case "ses":
		// Construct only the delivery client: campaign NewSES also requires feedback
		// infrastructure that tenant-less account and support messages do not use.
		client := sesv2.NewFromConfig(aws.Config{
			Region:      cfg.OutboundSESRegion,
			Credentials: credentials.NewStaticCredentialsProvider(cfg.OutboundSESAccessKeyID, cfg.OutboundSESSecretAccessKey, ""),
			Retryer:     func() aws.Retryer { return aws.NopRetryer{} },
		}, func(options *sesv2.Options) {
			if deps.HTTPClient != nil {
				options.HTTPClient = deps.HTTPClient
			}
			if deps.SESEndpointResolver != nil {
				options.EndpointResolverV2 = deps.SESEndpointResolver
			}
		})
		transport.Sender = apiSender{
			delivery: &provider.SES{Client: client, FromEmail: cfg.OutboundFromEmail, ConfigurationSet: cfg.OutboundSESConfigurationSet},
			from:     from, suppression: deps.Suppression,
		}
	}
	if cfg.OutboundFromEmail != "" {
		transport.Mailer = transactionalMailer{
			sender: transport.Sender, from: from, envelopeFrom: cfg.OutboundFromEmail,
			domain: cfg.OutboundFromEmail[strings.LastIndexByte(cfg.OutboundFromEmail, '@')+1:],
		}
	}
	return transport, nil
}

// safeSender prevents relay replies (including echoed credentials or content)
// from reaching worker logs, while preserving the suppression outcome.
type safeSender struct{ sender notify.Sender }

func (s safeSender) Send(ctx context.Context, message notify.Mail) error {
	if err := s.sender.Send(ctx, message); err != nil {
		if errors.Is(err, notify.ErrSuppressed) {
			return notify.ErrSuppressed
		}
		return notify.ErrNotAccepted
	}
	return nil
}

// apiSender keeps the configured provider identity separate from the business's
// reply routing. Provider-managed DKIM replaces any tenant-selected signing key.
type apiSender struct {
	delivery    provider.Deliverer
	from        string
	suppression mailer.SuppressionChecker
	structured  bool
}

func (s apiSender) Send(ctx context.Context, message notify.Mail) error {
	if err := checkSuppression(ctx, s.suppression, message.To); err != nil {
		return err
	}
	message.From = s.from
	message.EnvelopeFrom = ""
	message.DKIM = nil
	// Resend's structured API takes threading in custom headers. Clone the map
	// before adding these so concurrent sends never mutate the caller's message.
	if s.structured && (message.InReplyTo != "" || len(message.References) != 0) {
		message.ExtraHeaders = maps.Clone(message.ExtraHeaders)
		if message.ExtraHeaders == nil {
			message.ExtraHeaders = make(map[string]string, 2)
		}
		if message.InReplyTo != "" {
			message.ExtraHeaders["In-Reply-To"] = "<" + message.InReplyTo + ">"
		}
		if len(message.References) != 0 {
			message.ExtraHeaders["References"] = "<" + strings.Join(message.References, "> <") + ">"
		}
	}
	if _, err := s.delivery.Send(ctx, message); err != nil {
		// Provider responses and transport errors can contain credentials or message
		// content. Never expose them to account handlers or support worker logs.
		return notify.ErrNotAccepted
	}
	return nil
}

type transactionalMailer struct {
	sender       notify.Sender
	from         string
	envelopeFrom string
	domain       string
}

func (m transactionalMailer) Send(ctx context.Context, message mailer.Message) error {
	err := m.sender.Send(ctx, notify.Mail{
		From: m.from, EnvelopeFrom: m.envelopeFrom, To: message.To,
		Subject: message.Subject, BodyText: message.Body,
		MessageID: uuid.NewString() + "@" + m.domain,
	})
	if err != nil {
		if errors.Is(err, notify.ErrSuppressed) {
			return errors.Join(mailer.ErrNotAccepted, notify.ErrSuppressed)
		}
		return mailer.ErrNotAccepted
	}
	return nil
}

type developmentMailer struct {
	sink        mailer.LogMailer
	suppression mailer.SuppressionChecker
}

func (m developmentMailer) Send(ctx context.Context, message mailer.Message) error {
	if err := checkSuppression(ctx, m.suppression, message.To); err != nil {
		return errors.Join(mailer.ErrNotAccepted, err)
	}
	return m.sink.Send(ctx, message)
}

func checkSuppression(ctx context.Context, checker mailer.SuppressionChecker, recipient string) error {
	if checker == nil {
		return nil
	}
	suppressed, err := checker.IsSuppressed(ctx, recipient)
	if err != nil {
		return notify.ErrNotAccepted
	}
	if suppressed {
		return notify.ErrSuppressed
	}
	return nil
}
