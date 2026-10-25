# Support inbound mail via Resend

How a business receives support mail on its own subdomain through its Resend
account (spec 017). Replies go out through the same Resend profile, from the
custom address, with `Reply-To: support+<token>@support.<domain>`. They never
fall back to the shared transport.

## Before deploying this release (hub)

The server now refuses to start in production without
`MANYFORGE_INBOUND_REPLY_TOKEN_SECRET`. The Helm chart reads it from the
`inbound-reply-token` key of the hub's `manyforge-masterkeys` Secret
(`secrets.masterKeys.inboundReplyTokenKey`). The env entry is `optional: true`, so
a missing key does not stop the pod from being scheduled, but the process exits at
boot with `MANYFORGE_INBOUND_REPLY_TOKEN_SECRET: required in production` and the
rollout crash-loops. The migrate Job does not need the key.

Flux deploys the chart straight from `master`, so add the key **before** merging.
The value is used as raw bytes (no `base64:` prefix decoding), so any long random
string works:

```bash
export KUBECONFIG=~/.kube/talos
umask 077
secret_work=$(mktemp -d "$HOME/.manyforge-secret.XXXXXX")
trap 'rm -rf "$secret_work"' EXIT
openssl rand -base64 48 > "$secret_work/inbound-reply-token"
# The patch contains only the new data key, preserving the existing master keys.
kubectl -n manyforge create secret generic manyforge-masterkeys \
  --from-file=inbound-reply-token="$secret_work/inbound-reply-token" \
  --dry-run=client -o json > "$secret_work/patch.json"
kubectl -n manyforge patch secret manyforge-masterkeys --type=merge \
  --patch-file="$secret_work/patch.json"
```

Bootstrap automation must also read this key from an owner-only regular file via
`--from-file`, preserving existing keys. Never pass secret values as arguments or
include them in rendered dry-run output.

Consequence: reply tokens minted before this change were signed with an empty
key and no longer verify. Those replies still thread by `In-Reply-To`/`References`.

Optional, closes `manyforge-g97y`: set `blob.url` (for example
`s3://manyforge-attachments?region=us-east-1`) in the HelmRelease values. If the
bucket needs static credentials, set `secrets.blob.secretName` to a Secret holding
`access-key-id` and `secret-access-key`. Without it, every inbound attachment
is recorded as dropped and the thread shows it as "not stored".

## After deploying

- On boot, each replica runs a sweep that adds `email.received` to every verified
  Resend profile's webhook (`PATCH /webhooks/{id}`). Check the logs for
  `mailing Resend webhook subscribed to email.received` (one per profile, first
  boot only). A warning containing `signing secret changed` means that profile
  must be re-verified from **Mailing → Sending profile → Verify**.

## DNS: everything lives under `support.<domain>`

Every record for this feature sits under the `support.` subdomain. The apex
domain's MX and SPF records are **not touched**. For garden.gg this means the apex
`garden.gg` MX (Microsoft 365) and its SPF TXT stay exactly as they are, and mail
to `@garden.gg` keeps going to Microsoft 365.

### Cloudflare records to add (garden.gg example)

Add these in the `garden.gg` zone. Do not type Resend values from memory or
from this page: copy each value (and MX priority) exactly as shown in
**Resend → Domains → support.garden.gg**, and the verify token from the ManyForge
checklist. The names below are what Cloudflare shows.

| Type | Name | Value | Notes |
|---|---|---|---|
| `TXT` | `_manyforge.support` | `<mf-verify token from ManyForge>` | ManyForge domain ownership |
| `MX` | `support` | `<receiving MX value and priority from Resend>` | receiving; if other MX records exist on this name, give Resend's MX the lowest priority number (highest priority) so it is tried first |
| `TXT` | `resend._domainkey.support` | `<DKIM value from Resend>` | DKIM for replies sent from the subdomain |
| `MX` | `send.support` | `<feedback MX value and priority from Resend>` | Return-Path / bounces |
| `TXT` | `send.support` | `<SPF value from Resend>` | SPF for the Return-Path domain |

All records are **DNS only** (grey cloud). Cloudflare cannot proxy MX or TXT
records anyway.

## Onboarding a business (garden.gg example)

1. The business has a verified Resend sending profile (Mailing settings).
2. In Resend: **Domains → Add** `support.garden.gg`, enable **Receiving**.
3. In ManyForge: **Support → Email domains**, add `support.garden.gg` with mode
   `provider_route`. The page shows "Received via Resend" and the DNS checklist.
4. Publish every record from the table above in Cloudflare.
5. Verify the domain in Resend, then click **Verify** in ManyForge. A 409 means
   another business already verified that domain (see Troubleshooting).
6. Add the inbound address `support@support.garden.gg`.
7. Optional, to reply as the apex address ([addendum](../../specs/017-support-email-resend/addendum-send-as.md)):
   in Microsoft 365, route `support@garden.gg` to `support@support.garden.gg`
   with a mail flow rule (see [Microsoft 365: route the apex address](#microsoft-365-route-the-apex-address)).
   Then, in **Support → Inbox settings**, set **Send replies as** on
   `support@support.garden.gg` to `support@garden.gg`. This is only accepted
   when the business's verified Resend sending profile sends from `@garden.gg`.
   Replies then show `From: support@garden.gg`, and Reply-To stays
   `support+<token>@support.garden.gg`.

### Microsoft 365: route the apex address

Use an Exchange **mail flow rule**, not mailbox forwarding. Microsoft's outbound
anti-spam policy blocks automatic external forwarding by default ("Automatic -
System-controlled"), so mailbox forwarding bounces with `550 5.7.520 Access
denied, Your organization does not allow external forwarding`. Allowing it needs a
custom outbound policy, which in turn needs *organization customization*; the
"complete the setup process for your organization" prompt often errors out or takes
up to an hour. Admin mail flow rules are not subject to that policy and need no
setup.

1. Exchange admin center → **Recipients → Mailboxes → Add a shared mailbox**:
   `support@garden.gg` (no license needed). If the address is taken, remove the
   alias from whichever mailbox holds it first.
2. **Mail flow → Rules → Add a rule → Create a new rule**:
   - Name: `support@garden.gg → support portal`
   - Apply this rule if: **The recipient → is this person** → `support@garden.gg`
   - Do the following: **Add recipients → to the Bcc box** →
     `support@support.garden.gg` (keeps a copy in the shared mailbox; use
     **Redirect the message to** if you don't want one)
   - Save, then **Enable** the rule. Exchange saves new rules as **Disabled**; until
     you turn it on, mail just lands in the shared mailbox and never reaches Resend
     (check **Mail flow → Rules**: Status must read *Enabled*).
3. Leave **Mail flow settings → Email forwarding** on the mailbox **off**. With
   both the rule and forwarding, every message is delivered twice (and the
   forwarded copy bounces with 5.7.520).
4. Do **not** enable automatic forwarding in the *Default* outbound anti-spam
   policy instead: that allows external forwarding for every mailbox in the tenant.

### Note for spec 018 (support portal)

The future portal at `support.garden.gg` must use **proxied `A`/`AAAA` records**,
never a `CNAME`. A CNAME cannot coexist with any other record on the same name,
so it would conflict with the `MX` (and `TXT`) records on `support.garden.gg`
above and break inbound mail.

## Live verification (hub)

1. Send a real email to `support@garden.gg` (through the M365 mail flow rule) or
   straight to `support@support.garden.gg`. A ticket appears in garden.gg's queue.
2. Reply from the ticket. The customer receives it From `support@garden.gg` (the
   Send-as address; `support@support.garden.gg` without it) with
   `Reply-To: support+<token>@support.garden.gg`. The token is 45 chars, so the
   local part is 53 octets; Resend rejects a Reply-To over 64 octets with 422.
3. Reply from the customer's mail client. The message lands on the same ticket.
4. `dig MX garden.gg` and `dig TXT garden.gg` show the same values as before rollout.

## Observed: does Resend's webhook `to` preserve +token?

Observed in the garden.gg live round trip on 2026-10-12 (Resend us-east-1), from
the `mailing Resend inbound recipients observed` log line:

- Mail routed in by the M365 rule (to `support@support.garden.gg`, no tag): both
  `false`, as expected.
- Customer reply to `support+<token>@support.garden.gg`:
  `webhook_to_has_plus_tag=true`, `api_to_has_plus_tag=true`. The reply was sent
  with no In-Reply-To/References and still threaded onto the original ticket, so
  the `+token` survives Resend end to end.
- No `signing secret changed` warning was logged.

Routing does not depend on these answers. Recipients are read from the raw
message's `To`/`Cc` headers first; the authenticated Receiving API's
`received_for`/`to` are used only when no header recipient is owned. The webhook
payload's `to` is never used for routing.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Pod crash-loops with `MANYFORGE_INBOUND_REPLY_TOKEN_SECRET: required in production` | `inbound-reply-token` missing from `manyforge-masterkeys` | patch the Secret (above) |
| Resend shows webhook deliveries failing with 503 | Resend fetch/download failed, ingest error, or `Inbound` not wired | check the `mailing Resend received email fetch failed` / `download failed` / `inbound ingest failed` logs; Resend retries automatically |
| Error log `resend receiving credential rejected` | the profile's Resend API key was revoked or lacks receiving access | rotate the key in Mailing settings; mail is lost once Resend stops retrying |
| Mail accepted (200) but no ticket | no recipient owned by the profile's business, or the domain is unverified | look for `audit_entry.action = 'ticket.inbound_dropped'`; verify the domain and address |
| Reply shows **Delivery failed** | the business's Resend profile can't send (unverified, revoked key) | look for `send: reply failed on final attempt` and its `reason`; re-verify the sending profile. Replies never fall back to the shared transport |
| Verify on Email domains returns 409 | another business already verified that domain | domains are globally unique; use a different subdomain |

## Authenticated customer integrations

A tenant-root or inherited locked Owner with `tickets.write` may manage
`/api/v1/businesses/{id}/support/portal-keys`. Creation returns a server-only
`mfsp_` bearer once; only its SHA-256 hash is stored. Keys expire after 90 days by
default (1–365 days configurable), are revocable, and stop working when their
human creator is deactivated, deleted, or loses permission. Up to ten active keys
may exist per business. Rotate by issuing a replacement, updating the integrating
backend's Secret through an owner-only file, verifying its delivery, then revoking
the old key. Never embed this credential in a browser or use it as a URL parameter.

The integrating backend sends this bearer to `/api/v1/support/portal` with
`X-MF-External-User: tph:<canonical user UUID>` and an optional bounded display
name in `X-MF-External-Name`. These assertions must come from its own authenticated
session. A client-selected email, name, or UUID is not a verified customer identity.
Contact verification sends an eight-digit code through the exact business's
verified Resend support profile; the code expires in ten minutes, is single-use,
and allows six attempts. Resends require one minute and are capped at five/hour.
Send allowances commit before provider delivery; cancellation and ambiguous
provider results consume the allowance and preserve any prior pending/verified
contact proof. Only the matching latest accepted send may activate a new code.

Portal ownership is independent of notification addresses: email proof neither
imports existing email tickets nor grants access to another customer's thread.
Contact replacement updates notification recipients only for already-owned portal
tickets. Customer tickets/messages are native support records with normal
outbox/audit behavior. Internal notes and attachments are excluded. Thread reads
return newest-first pages, with `next_cursor` passed back as `before`; each page
contains at most 20 complete messages. Individual messages are capped at 1 MiB
before allocation; encoded pages are capped at 6 MiB plus envelope. Oversized
legacy messages return an error rather than silently truncating private content.
Consumers should allow an 8 MiB HTTP response cap and display pages chronologically.

Customer and management endpoints enforce admission/rates before body reads,
64 KiB strict JSON limits, and finite network/body deadlines. Validate creation,
resend failure/cancellation, creator lifecycle, two-user/two-business isolation,
native operator replies/internal-note denial, and real provider delivery before
calling an integration ready.

### Requesters shared by sibling businesses

A sender is deduplicated within the tenant. Business-only support staff can list,
read, and reply to their own tickets even when that sender first contacted another
business. The ticket exposes the notification email and requester ID, while sibling
directory names, CRM links, and activity timestamps remain private. Direct requester
directory endpoints and writes retain their original business-scoped RLS rules.
