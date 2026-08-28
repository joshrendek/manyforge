package provider

import (
	"context"
	"fmt"
	"net/mail"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"github.com/manyforge/manyforge/internal/platform/notify"
)

// SESAPI is the subset of the AWS SES v2 client used for delivery and profile verification.
type SESAPI interface {
	SendEmail(context.Context, *sesv2.SendEmailInput, ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
	GetEmailIdentity(context.Context, *sesv2.GetEmailIdentityInput, ...func(*sesv2.Options)) (*sesv2.GetEmailIdentityOutput, error)
	GetAccount(context.Context, *sesv2.GetAccountInput, ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error)
}

// SES delivers raw MIME and verifies both the configured identity and account.
type SES struct {
	Client           SESAPI
	FromEmail        string
	ConfigurationSet string
}

// NewSES validates static configuration and constructs an AWS SES v2 client.
func NewSES(ctx context.Context, profile Profile, endpoint sesv2.EndpointResolverV2, httpClient sesv2.HTTPClient) (*SES, error) {
	if strings.TrimSpace(profile.SESRegion) == "" {
		return nil, fmt.Errorf("provider: SES region is required")
	}
	if strings.TrimSpace(profile.SESAccessKeyID) == "" || strings.TrimSpace(profile.SESSecretAccessKey) == "" {
		return nil, fmt.Errorf("provider: SES static credentials are required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(profile.SESRegion),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(profile.SESAccessKeyID, profile.SESSecretAccessKey, "")),
		awsconfig.WithHTTPClient(httpClient),
		awsconfig.WithRetryer(func() aws.Retryer { return aws.NopRetryer{} }),
	)
	if err != nil {
		return nil, fmt.Errorf("provider: configure SES: %w", err)
	}
	client := sesv2.NewFromConfig(cfg, func(options *sesv2.Options) {
		if endpoint != nil {
			options.EndpointResolverV2 = endpoint
		}
	})
	return &SES{Client: client, FromEmail: profile.FromEmail, ConfigurationSet: profile.SESConfigurationSet}, nil
}

// Send submits shared, header-validated raw MIME to SES.
func (s *SES) Send(ctx context.Context, mail notify.Mail) (SendResult, error) {
	raw, err := notify.BuildMIME(mail)
	if err != nil {
		return SendResult{}, err
	}
	input := &sesv2.SendEmailInput{
		Content: &types.EmailContent{Raw: &types.RawMessage{Data: raw}},
	}
	if s.ConfigurationSet != "" {
		input.ConfigurationSetName = aws.String(s.ConfigurationSet)
	}
	out, err := s.Client.SendEmail(ctx, input)
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{ProviderID: aws.ToString(out.MessageId)}, nil
}

// Verify checks that the From domain is verified and account sending is enabled.
func (s *SES) Verify(ctx context.Context) error {
	address, err := mail.ParseAddress(s.FromEmail)
	if err != nil {
		return fmt.Errorf("provider: SES from address: %w", err)
	}
	at := strings.LastIndex(address.Address, "@")
	if at < 0 {
		return fmt.Errorf("provider: SES from address has no domain")
	}
	domain := address.Address[at+1:]
	identity, err := s.Client.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: aws.String(domain)})
	if err != nil {
		return err
	}
	if !identity.VerifiedForSendingStatus {
		return fmt.Errorf("provider: SES identity %s is not verified for sending", domain)
	}
	account, err := s.Client.GetAccount(ctx, &sesv2.GetAccountInput{})
	if err != nil {
		return err
	}
	if !account.SendingEnabled {
		return fmt.Errorf("provider: SES account sending is disabled")
	}
	return nil
}
