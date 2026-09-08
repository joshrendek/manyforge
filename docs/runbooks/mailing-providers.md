# Mailing provider operations

ManyForge supports the platform SMTP relay, Resend, and Amazon SES v2. Provider
credentials are sealed at rest with `MANYFORGE_MAILING_MASTER_KEY`; the plaintext
key belongs in the deployment's secret manager and never in Helm values.

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

Production requires a genuine SMTP relay by default. When email is not needed,
explicitly set `MANYFORGE_OUTBOUND_MAIL_DISABLED=true`, or set the top-level Helm
value `outboundMailDisabled: true`. The chart passes this setting to both the app
and the migration Job, so production startup does not require an SMTP host in
this mode. Keep the production environment; never substitute development mode or
a fake SMTP host for transport configuration.

The switch defaults to `false` and takes precedence over any configured relay or
provider credentials. It disables all outgoing transports: the platform SMTP
relay, Resend, and Amazon SES v2. Send attempts are rejected as not accepted, never
reported as delivered or silently discarded, and message contents and tokens are
not logged. Authentication emails and email invitations are unavailable while
disabled. This switch is separate from the mailing master key setting below.

To enable outbound mail later, configure a genuine SMTP relay and any required
credential secrets, then remove the environment flag or set it to `false` (Helm:
remove the override or set `outboundMailDisabled: false`) and roll out the release.
The app and migration Job again enforce the default production SMTP requirement;
configure any tenant Resend or SES profiles as described below.

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
