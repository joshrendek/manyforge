package provider

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/manyforge/manyforge/internal/platform/notify"
)

// Local verification failures carry only fixed, actionable text. Never construct
// these from provider responses: VerificationMessage is the disclosure boundary.
var (
	ErrCredentials           = errors.New("Provider credentials are missing or invalid. Save a valid Resend API key or SES access key and secret, then verify again.")
	ErrCredentialStorage     = errors.New("Stored mailing credentials could not be opened. Ask an administrator to check MANYFORGE_MAILING_MASTER_KEY and credential storage, then save the provider credentials again.")
	ErrSenderDomain          = errors.New("The sender domain is missing or unverified. Add and verify the From address domain with the selected provider, complete its DNS records, then verify again.")
	ErrSenderAddress         = errors.New("The From address is invalid. Save a valid email address on your verified sender domain, then verify again.")
	ErrSESSendingDisabled    = errors.New("SES account sending is disabled or suspended. Ask your AWS administrator to restore SES sending in the selected region, then verify again.")
	ErrSESConfiguration      = errors.New("The SES region or configuration set is missing or unavailable. Create a configuration set in the selected region and save its name, then verify again.")
	ErrSESFeedback           = errors.New("SES feedback does not match this profile. Configure an enabled configuration-set SNS destination for bounce and complaint events, using the saved topic in the same region and AWS account as the credentials, then verify again.")
	ErrResendWebhook         = errors.New("Resend webhook setup could not be confirmed. Use a full-access API key, check the public HTTPS callback is reachable, then verify again to reconcile the feedback webhook.")
	ErrResendWebhookLimit    = errors.New("Resend has too many webhooks to safely reconcile. Ask your Resend administrator to remove unused webhooks until the account fits within 100 entries, then verify again.")
	ErrRelayConfiguration    = errors.New("The relay is not configured. Select a verified email domain and ask an administrator to configure the SMTP relay and DKIM key, then verify again.")
	ErrDKIMKey               = errors.New("The relay DKIM key could not be opened or is invalid. Ask an administrator to check MANYFORGE_DKIM_MASTER_KEY and restore or regenerate the domain DKIM identity, publish its DNS record, then verify again.")
	ErrProviderConfiguration = errors.New("Provider verification is not configured on this instance. Ask an administrator to check the mailing provider configuration, then verify again.")
	ErrPublicURL             = errors.New("The public mailing callback URL is not configured. Ask an administrator to set MANYFORGE_PUBLIC_BASE_URL to the externally reachable HTTPS origin, then verify again.")
)

// VerificationMessage maps structured failures to bounded public diagnostics.
// Error text, API messages, request URLs and arbitrary provider codes are never
// interpolated. Wrapping the original errors remains safe for Classify callers.
func VerificationMessage(err error) string {
	if errors.Is(err, notify.ErrNotAccepted) {
		return "Outbound mail is disabled on this instance. Ask an administrator to review MANYFORGE_OUTBOUND_MAIL_DISABLED (Helm outboundMailDisabled); after an approved enablement, verify again."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Provider verification timed out. Retry verification; if it persists, ask an administrator to check provider connectivity and request timeouts."
	}
	if errors.Is(err, context.Canceled) {
		return "Provider verification was interrupted. Retry verification when the connection is stable."
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "InvalidClientTokenId", "UnrecognizedClientException", "InvalidSignatureException", "SignatureDoesNotMatch", "ExpiredToken", "ExpiredTokenException", "InvalidAccessKeyId", "MissingAuthenticationToken":
			return verificationAuthenticationMessage
		case "AccessDenied", "AccessDeniedException", "UnauthorizedException", "ForbiddenException":
			return verificationPermissionsMessage(err)
		case "AccountSuspendedException", "SendingPausedException":
			return ErrSESSendingDisabled.Error()
		case "MailFromDomainNotVerifiedException":
			return ErrSenderDomain.Error()
		case "NotFoundException", "NotFound":
			var requestErr *sesRequestError
			if errors.As(err, &requestErr) {
				switch requestErr.operation {
				case "identity verification":
					return ErrSenderDomain.Error()
				case "configuration-set verification", "event-destination verification":
					return ErrSESConfiguration.Error()
				}
			}
		case "TooManyRequestsException", "Throttling", "ThrottlingException", "LimitExceededException":
			return verificationRateLimitMessage
		case "RequestTimeout", "RequestTimeoutException":
			return "The provider timed out during verification. Wait briefly and verify again; ask an administrator to check connectivity if it persists."
		}
	}
	var httpErr *HTTPError
	var responseErr *smithyhttp.ResponseError
	status := 0
	if errors.As(err, &httpErr) {
		status = httpErr.StatusCode
		switch httpErr.Code {
		case "invalid_api_key", "missing_api_key":
			return verificationAuthenticationMessage
		case "restricted_api_key":
			return verificationPermissionsMessage(err)
		}
	} else if errors.As(err, &responseErr) {
		status = responseErr.HTTPStatusCode()
	}
	switch {
	case status == http.StatusUnauthorized:
		return verificationAuthenticationMessage
	case status == http.StatusForbidden:
		return verificationPermissionsMessage(err)
	case status == http.StatusTooManyRequests:
		return verificationRateLimitMessage
	case status == http.StatusRequestTimeout || status >= 500 && status <= 599:
		return verificationUnavailableMessage
	}
	if apiErr != nil && apiErr.ErrorFault() == smithy.FaultServer {
		return verificationUnavailableMessage
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "Provider verification timed out. Retry verification; if it persists, ask an administrator to check provider connectivity and request timeouts."
		}
		return "The provider could not be reached. Retry verification; if it persists, ask an administrator to check DNS, TLS and outbound network access."
	}
	for _, known := range []error{
		ErrCredentials, ErrCredentialStorage, ErrSenderDomain, ErrSenderAddress,
		ErrSESSendingDisabled, ErrSESConfiguration, ErrSESFeedback, ErrResendWebhookLimit,
		ErrRelayConfiguration, ErrDKIMKey, ErrProviderConfiguration, ErrPublicURL, ErrResendWebhook,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "Provider verification failed. Check the saved provider settings and provider service status, then verify again; contact an administrator if the problem persists."
}

const (
	verificationAuthenticationMessage = "The provider rejected the credentials. Replace the Resend API key or SES access key and secret with active credentials for the correct account, then verify again."
	verificationRateLimitMessage      = "The provider rate limit was reached. Wait before verifying again; if it persists, ask your provider administrator to review account limits."
	verificationUnavailableMessage    = "The provider is temporarily unavailable. Check the provider service status and retry verification later."
)

func verificationPermissionsMessage(err error) string {
	var requestErr *sesRequestError
	if errors.As(err, &requestErr) {
		return "AWS denied verification access. Ask your AWS administrator to grant ses:GetEmailIdentity, ses:GetAccount, ses:GetConfigurationSet, ses:GetConfigurationSetEventDestinations and sts:GetCallerIdentity for this profile, then verify again."
	}
	return "The provider denied access. For Resend, use a full-access API key that can read domains and manage webhooks; for SES, ask your AWS administrator to grant identity, account, configuration-set and STS read access. Then verify again."
}
