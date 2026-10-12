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
kubectl -n manyforge patch secret manyforge-masterkeys --type=merge \
  -p "{\"stringData\":{\"inbound-reply-token\":\"$(openssl rand -base64 48)\"}}"
kubectl -n manyforge get secret manyforge-masterkeys -o jsonpath='{.data.inbound-reply-token}' | base64 -d | wc -c   # > 0
```

Also add the key to the `manyforge-masterkeys` block of
`~/dev/infra-k8s/docs/manyforge/deploy-secrets.sh` so a rebuilt cluster gets it:

```bash
  --from-literal=inbound-reply-token="$(openssl rand -base64 48)"
```

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
   in Microsoft 365, forward `support@garden.gg` to `support@support.garden.gg`
   (a mail flow rule or mailbox forwarding that keeps the original message).
   Then, in **Support → Inbox settings**, set **Send replies as** on
   `support@support.garden.gg` to `support@garden.gg`. This is only accepted
   when the business's verified Resend sending profile sends from `@garden.gg`.
   Replies then show `From: support@garden.gg`, and Reply-To stays
   `support+<token>@support.garden.gg`.

### Note for spec 018 (support portal)

The future portal at `support.garden.gg` must use **proxied `A`/`AAAA` records**,
never a `CNAME`. A CNAME cannot coexist with any other record on the same name,
so it would conflict with the `MX` (and `TXT`) records on `support.garden.gg`
above and break inbound mail.

## Live verification (hub)

1. Send a real email to `support@support.garden.gg`. A ticket appears in garden.gg's queue.
2. Reply from the ticket. The customer receives it from `support@support.garden.gg`
   with `Reply-To: support+<token>@support.garden.gg`, DKIM-aligned to the subdomain.
3. Reply from the customer's mail client. The message lands on the same ticket.
4. `dig MX garden.gg` and `dig TXT garden.gg` show the same values as before rollout.

## Observed: does Resend's webhook `to` preserve +token?

> **PLACEHOLDER — fill in after the live test.** Nothing below has been observed yet.

On live-verification step 3, search the logs for
`mailing Resend inbound recipients observed` and record what you see:

- Webhook payload `to` keeps `+token` tags (`webhook_to_has_plus_tag`): **TBD**
- Receiving API `to` keeps `+token` tags (`api_to_has_plus_tag`): **TBD**
- `PATCH /webhooks/{id}` kept the signing secret (no `signing secret changed` warning): **TBD**
- Observed on (date, Resend region): **TBD**

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
