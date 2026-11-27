# Portal custom domains

How a business serves its public customer pages on its own hostname, for example
`portal.example.com`, instead of the instance hostname (for example
`app.example.com`).

## What a custom hostname serves

- The public feedback pages (`/p/<key>`) and their API.
- The public mailing subscribe page (`/m/s/<key>`) and its API.
- The mailing confirmation link (`/m/confirm/<token>`).
- The static web-app assets those pages need.

Each key or confirmation token only works on the hostname of the business that
owns it. On any other hostname it behaves like an unknown key.

These are **not** served on a custom hostname (the response is 404): the operator
app, login, the authenticated API, `/metrics`, CLI downloads, the analytics
snippet and collect endpoints, webhooks, and the unsubscribe, open, click and logo
links. `/` is also 404 until a support portal home page exists.

Only the mailing double-opt-in confirmation link is sent on the custom hostname.
Unsubscribe, tracking and logo links always use the instance hostname, so removing
a domain cannot break mail that was already delivered.

## Onboarding a hostname

Until self-service registration exists, the operator does steps 1 and 2 for each
hostname and the business does step 3.

1. **DNS.** In Cloudflare, create a proxied (orange cloud) record for
   `portal.example.com` that points where `app.example.com` points.
2. **Ingress.** Add the hostname to `ingress.extraHosts` in the deployment's
   values and roll out:

   ```yaml
   ingress:
     extraHosts:
       - portal.example.com
   ```

   Each entry becomes one more rule on the existing Ingress, so its source-range
   restriction, `/metrics` denial and body-size and timeout limits apply to every
   hostname. Only hostnames listed here reach the app. The render fails if an
   entry equals `ingress.host` (case-insensitive) or appears twice.
3. **Verify.** In **Support → Inbox settings → Portal domain** the business adds
   `portal.example.com`, publishes a TXT record named
   `_manyforge-portal.portal.example.com` with the value shown, and clicks
   **Verify**.

Two checks before step 3:

- **TLS mode.** The origin is HTTP behind the edge. The hostname's DNS zone must
  use the same TLS mode to the origin as the instance hostname's zone. If the
  zone is stricter, add a per-hostname rule that relaxes it.
- **CNAME scope.** A CNAME to the instance hostname only works when both zones are
  in the same edge account. Otherwise use an address record.

## Verification and takeover

A hostname can be pending for several businesses and verified for only one.

Another business can take over a verified hostname only by proving control
through the TXT record while the current holder's TXT record is no longer
published. The previous holder reverts to pending. Both changes are audit-logged.

## Removing a hostname

1. Delete the domain in **Support → Inbox settings → Portal domain**.
2. Remove the hostname from `ingress.extraHosts` and roll out.
3. Delete the DNS record.

Order matters little. Deleting the domain in settings is what stops serving: the
pod that handled the deletion stops serving the hostname immediately, and every
other pod within 60 seconds (the lookup cache TTL).

## Troubleshooting

The app decides by the request's `Host` header only (lower-cased, port and one
trailing dot stripped). Forwarded-host headers are ignored.

| Response on the custom hostname | Meaning |
|---|---|
| 404 | The path is not on the allowlist, the hostname is not verified, or it was removed. |
| 503 with code `UNAVAILABLE` | The hostname lookup could not reach the database. The instance hostname is not affected. |
| No response from the app (edge or ingress error) | The hostname is missing from `ingress.extraHosts`, or the DNS record is missing or not proxied. |

Requests addressed to an IP literal, `localhost`, any single-label name, or a name
ending `.localhost`, `.svc` or `.svc.cluster.local` are treated as the instance
hostname, so probes and in-cluster callers keep working. The two-label
`service.namespace` short form is **not** recognised: in-cluster callers must use
the bare service name, an IP, or the full `.svc` name.

## Load

Each new, validly shaped hostname costs one database lookup per 60 seconds. Only
hostnames listed in `ingress.extraHosts` reach the app, so this is bounded. As
defence in depth, set a rate limit at the edge.
