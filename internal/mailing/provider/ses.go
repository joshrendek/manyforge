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
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/manyforge/manyforge/internal/platform/notify"
)

// SESAPI is the subset of the AWS SES v2 client used for delivery and profile verification.
type SESAPI interface {
	SendEmail(context.Context, *sesv2.SendEmailInput, ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
	GetEmailIdentity(context.Context, *sesv2.GetEmailIdentityInput, ...func(*sesv2.Options)) (*sesv2.GetEmailIdentityOutput, error)
	GetAccount(context.Context, *sesv2.GetAccountInput, ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error)
	GetConfigurationSet(context.Context, *sesv2.GetConfigurationSetInput, ...func(*sesv2.Options)) (*sesv2.GetConfigurationSetOutput, error)
}

type STSAPI interface {
	GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// SES delivers raw MIME and verifies both the configured identity and account.
type SES struct {
	Client            SESAPI
	Identity          STSAPI
	FromEmail         string
	ConfigurationSet  string
	ExpectedAccountID string
}

type sesRequestError struct {
	operation string
	cause     error
}

func (e *sesRequestError) Error() string { return "provider: SES " + e.operation + " failed" }
func (e *sesRequestError) Unwrap() error { return e.cause }

// NewSES validates static configuration and constructs an AWS SES v2 client.
func NewSES(ctx context.Context, profile Profile, endpoint sesv2.EndpointResolverV2, httpClient sesv2.HTTPClient) (*SES, error) {
	if strings.TrimSpace(profile.SESRegion) == "" {
		return nil, fmt.Errorf("provider: SES region is required")
	}
	if strings.TrimSpace(profile.SESAccessKeyID) == "" || strings.TrimSpace(profile.SESSecretAccessKey) == "" {
		return nil, fmt.Errorf("provider: SES static credentials are required")
	}
	if strings.TrimSpace(profile.SESConfigurationSet) == "" {
		return nil, fmt.Errorf("provider: SES configuration set is required")
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
	expectedAccountID := ""
	if topicARN := strings.TrimSpace(profile.SNSTopicARN); topicARN != "" {
		parts := strings.SplitN(topicARN, ":", 6)
		if len(parts) != 6 || parts[2] != "sns" || parts[3] != profile.SESRegion ||
			len(parts[4]) != 12 {
			return nil, fmt.Errorf("provider: SES SNS topic ARN does not match the configured region and account")
		}
		expectedAccountID = parts[4]
	}
	return &SES{
		Client: client, Identity: sts.NewFromConfig(cfg),
		FromEmail: profile.FromEmail, ConfigurationSet: profile.SESConfigurationSet,
		ExpectedAccountID: expectedAccountID,
	}, nil
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
		return SendResult{}, &sesRequestError{operation: "send", cause: err}
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
		return &sesRequestError{operation: "identity verification", cause: err}
	}
	if !identity.VerifiedForSendingStatus {
		return fmt.Errorf("provider: SES identity %s is not verified for sending", domain)
	}
	account, err := s.Client.GetAccount(ctx, &sesv2.GetAccountInput{})
	if err != nil {
		return &sesRequestError{operation: "account verification", cause: err}
	}
	if !account.SendingEnabled {
		return fmt.Errorf("provider: SES account sending is disabled")
	}
	if _, err := s.Client.GetConfigurationSet(ctx, &sesv2.GetConfigurationSetInput{
		ConfigurationSetName: aws.String(s.ConfigurationSet),
	}); err != nil {
		return &sesRequestError{operation: "configuration-set verification", cause: err}
	}
	if s.ExpectedAccountID != "" {
		identity, err := s.Identity.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			return &sesRequestError{operation: "account identity verification", cause: err}
		}
		if aws.ToString(identity.Account) != s.ExpectedAccountID {
			return fmt.Errorf("provider: SES SNS topic account does not match the credential account")
		}
	}
	return nil
}
