package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"

	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

// Factory owns the shared dependencies used to construct provider clients.
type Factory struct {
	DB                  *db.DB
	DKIMSealer          KeyOpener
	RelaySender         notify.Sender
	HTTPClient          *http.Client
	ResendBaseURL       string
	SESEndpointResolver sesv2.EndpointResolverV2
}

// Build constructs the relay, Resend, or SES client selected by profile.Mode.
func (f *Factory) Build(ctx context.Context, profile Profile) (Deliverer, error) {
	switch profile.Mode {
	case "relay":
		if profile.EmailDomainID == nil {
			return nil, fmt.Errorf("provider: relay profile has no email domain")
		}
		return &Relay{DB: f.DB, Sealer: f.DKIMSealer, Sender: f.RelaySender, EmailDomainID: *profile.EmailDomainID}, nil
	case "resend":
		return &Resend{APIKey: profile.ResendAPIKey, FromEmail: profile.FromEmail, BaseURL: f.ResendBaseURL, Client: f.HTTPClient}, nil
	case "ses":
		return NewSES(ctx, profile, f.SESEndpointResolver, f.HTTPClient)
	default:
		return nil, fmt.Errorf("provider: unknown mode %q", profile.Mode)
	}
}
