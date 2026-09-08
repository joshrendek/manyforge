# Mailing provider operations

ManyForge supports SMTP, Resend, and Amazon SES v2. Shared support/system transport
credentials come from instance-managed Secrets. Business mailing-profile credentials
are separately sealed at rest with `MANYFORGE_MAILING_MASTER_KEY`. Never commit
credential values to Helm values or expose them in logs.

## SMTP relay versus provider APIs

Resend and Amazon SES mailing profiles send through HTTPS APIs. They need provider
credentials, a verified sender, and ready feedback, but **no SMTP host, port, or
password**. SMTP and the instance DKIM key checks apply only to ManyForge relay.
Production startup and migrations work without an SMTP host.

Shared support, account-verification, password/email-change, and invitation mail
use one selected instance transport: **SMTP OR Resend OR SES**. There is no
automatic fallback to a different provider after a failure. API transports use
the configured system From identity and preserve support Reply-To tokens,
Message-ID, In-Reply-To, and References. SMTP support keeps its existing
per-business identity and optional DKIM signing.

Business mailing profiles remain independent. Their credentials are never
implicitly used for tenant-less account emails.

## Configure shared support and system mail

Set `outboundMail.provider` to `smtp`, `resend`, or `ses`, and configure a verified
system sender in `outboundMail.fromEmail` and optional `outboundMail.fromName`.
The corresponding environment variables are `MANYFORGE_OUTBOUND_PROVIDER`,
`MANYFORGE_OUTBOUND_FROM_EMAIL`, and `MANYFORGE_OUTBOUND_FROM_NAME`.

| Selected provider | Required settings                                                       | Credential source                                                 |
| ----------------- | ----------------------------------------------------------------------- | ----------------------------------------------------------------- |
| `smtp`            | `smtp.host`, `smtp.port`; system From for account/invitation mail       | Optional `secrets.smtp` username/password                         |
| `resend`          | System From and a sending API key                                       | `secrets.outboundMail.secretName`, key `resend-api-key`           |
| `ses`             | System From, `outboundMail.sesRegion`, static AWS access key and secret | Same Secret, keys `ses-access-key-id` and `ses-secret-access-key` |

For shared SES delivery, `outboundMail.sesConfigurationSet` is optional. SNS
subscription and campaign-feedback verification are not prerequisites for these
system/support messages. The business campaign setup below retains its separate
feedback requirements.

For example, after provisioning an instance-owned Resend Secret in the release
namespace, configure the HelmRelease:

```yaml
outboundMailDisabled: false
outboundMail:
  provider: resend
  fromEmail: accounts@example.com
  fromName: ManyForge
secrets:
  outboundMail:
    secretName: manyforge-system-mail
```

The example address must be replaced with an identity verified in your provider
account. The Secret's `resend-api-key` must be provisioned before the rollout.
No SMTP fields are needed. Only the selected provider's Secret references are
mounted; the application and pre-install/pre-upgrade migration Job receive the
same configuration.

Direct environment equivalents for API credentials are
`MANYFORGE_OUTBOUND_RESEND_API_KEY`, or `MANYFORGE_OUTBOUND_SES_REGION`,
`MANYFORGE_OUTBOUND_SES_ACCESS_KEY_ID`, and
`MANYFORGE_OUTBOUND_SES_SECRET_ACCESS_KEY`. The optional SES set is
`MANYFORGE_OUTBOUND_SES_CONFIGURATION_SET`.

With shared SMTP selected but no SMTP host, production shared mail is unavailable;
business API mailing profiles can still operate. Without a system From address,
SMTP support can operate but account/invitation mail is rejected. Production
never substitutes message/token logging for delivery. Global suppression is
checked before sending, and rejected messages are not acknowledged as sent.

## Guided sending setup

Open **Mailing → Sending profile** (`/mailing/sending`) for the five-stage setup:

1. **Instance:** review provider-scoped checks for outbound enablement, the mailing
   master key, public URL, SMTP relay, and relay DKIM master key. Blocked checks
   identify the environment/Helm setting an instance administrator must change.
   Refresh checks after the administrator rolls out the deployment. These checks
   confirm configuration, not network connectivity or delivery.
2. **Provider:** choose relay, Resend, or SES and enter the provider credentials.
   Existing credentials remain write-only; use Replace credentials to rotate them.
3. **Sender & DNS:** set the sender identity and postal address. Relay domains link
   to Support settings and show DNS instructions, observed verification state,
   and a Check DNS action. Resend and SES link to their provider domain consoles.
4. **Feedback:** save changes, then Verify provider and feedback. Provider
   verification and feedback readiness are separate results. Resend provisions
   its webhook automatically; SES needs the configuration set, SNS destination,
   and confirmed HTTPS subscription described below.
5. **Test delivery:** send to an address you control only after the instance,
   saved provider, and feedback checks are ready. Success means the provider
   accepted the message; check the inbox and spam folder to confirm delivery.

The wizard does not change instance configuration or enable outbound mail.
Profiles can be configured while outbound mail is disabled, provided the mailing
key is present. Unsaved edits must be saved before verification or test sending.
Verification failures give bounded corrective instructions without exposing
credentials, internal request URLs, or provider response payloads.

## Running without outbound mail

When email is not needed, explicitly set `MANYFORGE_OUTBOUND_MAIL_DISABLED=true`,
or set the top-level Helm value `outboundMailDisabled: true`. The chart passes this
setting to both the app and migration Job. Keep the production environment;
never substitute development mode or a fake SMTP host for transport configuration.

The switch defaults to `false` and takes precedence over any configured relay or
provider credentials. It disables all outgoing transports: the platform SMTP
relay, Resend, and Amazon SES v2. Send attempts are rejected as not accepted, never
reported as delivered or silently discarded, and message contents and tokens are
not logged. Authentication emails and email invitations are unavailable while
disabled. This switch is separate from the mailing master key setting below.

To enable outbound mail, remove the environment flag or set it to `false` (Helm:
remove the override or set `outboundMailDisabled: false`) and roll out the release.
Configure the chosen shared provider above and any independent business mailing
profiles below. Resend and SES require no SMTP settings. Use SMTP settings only
when selecting the SMTP transport or the business ManyForge relay option.

## Platform relay

1. Configure the shared SMTP transport with `MANYFORGE_SMTP_HOST`, port, and its
   optional username/password secret.
2. Configure `MANYFORGE_DKIM_MASTER_KEY`. Relay profiles reuse the existing email
   domain flow; a domain must be verified and have its tenant Ed25519 DKIM key.
3. Publish the relay operator's SPF include or IP authorization for the sending
   domain. The exact value comes from the SMTP relay operator.
4. Save a relay sending profile whose From address uses that verified domain, then
   run Verify and Send test in Mailing settings.

Relay delivery fails closed if the domain is no longer verified or its DKIM key
cannot be opened. It never falls back to unsigned tenant mail.

## Resend

1. Add and verify the sending domain in Resend.
2. Create an API key with access to send mail, read domains, and manage webhooks
   (Resend full access, rather than a sending-only key).
3. Save the profile in Resend mode and run Verify provider and feedback.
   ManyForge checks `GET /domains`, creates or reconciles the profile's webhook,
   and seals the provider-generated signing secret. Do not enter a webhook secret
   manually.
4. The callback is `/api/v1/inbound/mailing/{profileID}/resend` on the configured
   public origin. Make it externally reachable. ManyForge subscribes to
   `email.delivered`, `email.bounced`, `email.complained`, and
   `email.delivery_delayed`, verifies Svix signatures against the exact request
   bytes, and deduplicates on `svix-id`.
5. Once provider and feedback are ready, send a test. Messages use `POST /emails`
   with the mailing delivery ID supplied as Resend's `Idempotency-Key`.

## Amazon SES v2

1. Verify the From domain in the same AWS region configured on the profile.
2. Create an IAM principal restricted to the required identities and grant:
   `ses:SendEmail`, `ses:SendRawEmail`, `ses:GetEmailIdentity`, `ses:GetAccount`,
   `ses:GetConfigurationSet`, `ses:GetConfigurationSetEventDestinations`, and
   `sts:GetCallerIdentity`.
3. If the account is still in the SES sandbox, test recipients must also be
   verified. Request production access before a real campaign.
4. Create an SES configuration set with an enabled SNS event destination for
   Bounce and Complaint; Delivery events are also recommended. Create an HTTPS SNS subscription to the profile's
   inbound SES webhook URL, leave raw message delivery disabled, and save the SNS
   topic ARN on the profile. The URL is
   `/api/v1/inbound/mailing/{profileID}/ses`; ManyForge verifies the SNS signing
   certificate and requires the signed `TopicArn` to match the saved value before
   automatically following a signed subscription-confirmation URL.
5. Save the access key, secret, region, configuration-set name, and SNS topic ARN.
   Verify checks the sending identity and account sending status, configuration
   set, event destination, and topic region/account against the credentials.
   Feedback remains pending until ManyForge confirms the HTTPS SNS subscription.
   If the wizard's suggested callback origin differs from the configured public
   origin, use the administrator-configured origin with the same `/api/v1` path.
6. Re-run verification after correcting settings. Both provider verification and
   feedback readiness must pass before test sending is available.

SES sends the MIME generated by ManyForge as raw content so headers such as
List-Unsubscribe survive. SES returns its own message ID; provider events correlate
on that ID rather than ManyForge's original Message-ID.

## Deployment settings

Credential-dependent mailing APIs are disabled when `MANYFORGE_MAILING_MASTER_KEY`
is absent. The authenticated, `mailing.read`-gated
`GET /api/v1/businesses/{id}/mailing/setup` remains available so the wizard can
explain missing configuration. The Helm chart reads the key from
`secrets.masterKeys.mailingKey` with an optional Secret reference. Preserve
existing master keys when recovering a deployment. Non-secret controls are:

| Environment variable               |               Default | Purpose                                                                                  |
| ---------------------------------- | --------------------: | ---------------------------------------------------------------------------------------- |
| `MANYFORGE_OUTBOUND_MAIL_DISABLED` |               `false` | Reject all outgoing mail, including relay, Resend, and SES; Helm: `outboundMailDisabled` |
| `MANYFORGE_MAILING_RATE_RPS`       |                  `10` | Per-business campaign refill rate                                                        |
| `MANYFORGE_MAILING_RATE_BURST`     |                  `50` | Per-business campaign burst                                                              |
| `MANYFORGE_MAILING_SEND_BATCH`     |                 `100` | Deliveries claimed per worker pass                                                       |
| `MANYFORGE_MAILING_SEND_EVERY`     |                  `2s` | Worker polling interval                                                                  |
| `MANYFORGE_MAILING_LEASE`          |                  `2m` | Delivery claim lease                                                                     |
| `MANYFORGE_MAILING_MESSAGE_DOMAIN` | inbound system domain | Domain used in generated Message-ID values                                               |
| `MANYFORGE_PUBLIC_BASE_URL`        |                  none | Required public origin for confirmation, unsubscribe, click, and open links              |

Delivery is at least once: a process crash after a provider accepts a message but
before completion is recorded can cause a retry. Resend deduplicates the stable
delivery ID; SES and relay recipients may receive a duplicate in that narrow window.
