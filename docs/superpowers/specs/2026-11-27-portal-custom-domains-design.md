# Custom portal domains per business: design

Date: 2026-11-27. Status: approved by the owner. Tracker: `manyforge-20cz`.

## Intent

A business's customers should reach its public pages on the business's own
hostname, for example `support.example.com`, not on the instance hostname.
The reason is brand: a customer of one business should not see, or need to
know about, the shared instance.

Today that is not possible, and not safe to fake:

- The ingress has a single host rule (`charts/manyforge/templates/ingress.yaml`).
- The binary serves the web app and the API from one origin and knows one
  public address (`MANYFORGE_PUBLIC_BASE_URL`). Routing a second hostname to
  it unchanged would serve the whole operator app there, login included.

## Goals

- A business can register a hostname, prove it controls it, and have its
  public customer pages served there.
- The instance hostname behaves exactly as it does today.
- A custom hostname exposes nothing an anonymous customer should not see.
- Links in mail sent on a business's behalf use its hostname.

## Non-goals

- The hosted customer support portal itself (`manyforge-ysri`, designed
  separately and built after this).
- Registering hostnames with the edge provider automatically, and support
  for hostnames whose DNS the operator cannot proxy (`manyforge-2oqt`).
- Per-business theming or branding of the pages.
- Custom hostnames for the operator app or the authenticated API.

## Anti-goals

- The operator login, the authenticated API, `/metrics` or the CLI downloads
  answer on a custom hostname.
- An operator session established on the instance hostname is honoured on a
  custom hostname, or the reverse.
- A business can claim a hostname it does not control, or one that another
  business has verified.
- A request for a hostname nobody registered reaches any page.

## Constraints

- The edge proxy terminates TLS in front of the cluster; the origin is HTTP
  and the ingress only admits the edge's source ranges. That restriction
  must cover custom hostnames too.
- The `Host` header is attacker-controlled. It selects a business only after
  normalisation (lower-case, no port, no trailing dot) and an exact match
  against a verified record.
- Tenant isolation rules apply to the new table as to every other
  business-scoped table (row-level security, composite foreign keys).
- The existing email-domain feature already has a DNS TXT challenge
  (`IdentityService.VerifyEmailDomain`, `NetTXTResolver`). Reuse the
  mechanism; do not invent a second one.

## Approaches considered

The app-side design had one sensible shape. The alternatives were at the
edge, for getting a certificate and traffic for a hostname:

1. **Proxied DNS record created by the operator (chosen for now).** Works
   whenever the hostname's DNS zone is at the same edge provider under the
   operator's control. No new provider feature, nothing to pay for, and the
   origin stays hidden. Each hostname is a manual record plus one line of
   chart values.
2. **The edge provider's custom-hostname product.** The business adds one
   CNAME and the instance registers the hostname through the provider's API.
   Needed for hostnames whose DNS is elsewhere. More moving parts (API
   credential, certificate status tracking); deferred to `manyforge-2oqt`.
3. **Terminate TLS in the cluster on an entry point outside the edge.**
   Certificates per hostname from the in-cluster issuer. Exposes the origin
   address, loses the edge's protection, and needs a second trust path for
   client addresses. Rejected.

The app-side work below is the same for 1 and 2, so moving to 2 later does
not rework it.

## Design

### Data

New table `portal_domain`, business-scoped:

- `hostname` (case-insensitive), `verify_token`, `verified_at`, timestamps,
  `business_id`, `tenant_root_id`.
- A hostname may be pending for more than one business, but verified for
  only one: a partial unique index on `hostname` where `verified_at` is not
  null. Verification of an already verified hostname fails as a conflict.
- Hostname validation at the service boundary: a fully qualified DNS name,
  at most 253 characters, no wildcard, no IP literal, not the instance
  hostname or any platform-owned mail domain.

### Verification

The business publishes a TXT record at `_manyforge-portal.<hostname>`
containing the token. Verify looks it up with the existing resolver and
stamps `verified_at`. Removing a domain deletes the record and takes effect
on the next lookup.

### Routing

One middleware ahead of every route decides by normalised host:

| Host | Behaviour |
|---|---|
| The instance host (from `MANYFORGE_PUBLIC_BASE_URL`), or no public base URL configured | Unchanged. |
| An internal name: IP literal, `localhost`, or a name ending `.localhost`, `.svc` or `.svc.cluster.local` | Treated as the instance host. Probes and in-cluster callers use these; they cannot be registered and cannot arrive through the edge. |
| A verified portal domain | The business is put in the request context. Only the public allowlist is served; everything else is 404. |
| Anything else | 404, with no detail. |

`/healthz` and `/readyz` stay reachable regardless of host, because probes
do not send the public hostname.

The public allowlist is explicit, kept in one place, and starts as: the
public feedback pages and their API, the public mailing pages and their API,
the static assets the web app needs to render those pages, and the analytics
snippet and collect endpoint. The support portal adds its own entries when it
ships. A public endpoint that takes a key resolving to a business (a feedback
board key, a mailing list key) must match the business the hostname resolved
to, or behave as for an unknown key. Token links are tied too (owner
decision): the mailing confirmation link is the only token link generated on
a custom hostname, so it is the only token route served there, and it only
activates a subscription belonging to the hostname's business. Unsubscribe,
open, click and brand-logo routes are not served on custom hostnames; their
links are always generated on the instance hostname. The root path `/` is
404 on a custom hostname until the support portal defines a home page.

The verified-hostname lookup is cached in memory with a short expiry (on the
order of a minute), so a request does not cost a query and removal is prompt.

### Sessions

Operator sessions are bearer tokens kept in per-origin browser storage, so a
browser never presents them on another hostname. The only cookie, used during
passkey sign-in, carries the `__Host-` prefix when secure and so cannot be
scoped to a wider domain. The middleware does not rely on either: on a custom
hostname the authenticated routes are not reachable at all. A test pins both.

### Ingress

`ingress.extraHosts` in chart values: a list of additional hostnames rendered
as extra rules on the same Ingress, so the source-range restriction, the
`/metrics` denial and the body-size limits apply unchanged. Empty by default.

### Links in mail

The mailing double-opt-in confirmation link uses
`https://<verified portal hostname>` when the business has one. With more
than one verified hostname, the earliest verified wins.

Unsubscribe, open-tracking, click-tracking and brand-logo links stay on the
instance hostname. They sit in delivered mail indefinitely, and a domain that
is later removed or whose DNS lapses must not break an unsubscribe link or
every link in an old message. Customer-facing support links are decided with
the support portal.

### Settings and API

Business settings gain a "Portal domain" section, gated by the existing
inbox-management permission: add a hostname, see the TXT record to publish
and the DNS record to point at the instance, verify, see status, remove.
Endpoints are added to the OpenAPI contract with list, create, verify and
delete operations.

### Operating it (until `manyforge-2oqt`)

For each hostname the operator creates a proxied DNS record pointing where
the instance hostname points and adds the hostname to `ingress.extraHosts`.
The runbook notes two things to check: the hostname's zone must use the same
TLS mode to the origin as the instance's zone, and a CNAME to the instance
hostname only works when both zones are in the same edge account (otherwise
use an address record).

## Tests

- **Service, against Postgres:** create, verify with a fake resolver (match,
  mismatch, lookup failure), verified-hostname uniqueness across tenants,
  pending duplicates allowed, delete, validation rejects, tenant isolation
  and permission checks, foreign and unknown ids return the same not-found.
- **Routing:** instance host unchanged; verified host serves each allowlist
  entry and 404s operator login, authenticated API, `/metrics`, CLI
  downloads; unverified and unknown hosts 404; host normalisation (case,
  port, trailing dot); a public key belonging to another business 404s on
  this hostname; health endpoints on any host; cache expiry after removal.
- **Security regression:** one file pinning that authenticated routes are
  unreachable on a custom hostname and that the session cookie has no
  `Domain` attribute.
- **Mail links:** the confirmation link uses the verified hostname when
  present and the instance URL otherwise; unsubscribe, tracking and logo
  links always use the instance URL.
- **Chart:** rendering with and without `extraHosts`; restrictions present
  on every rule.
- **Browser (Playwright, mocked API):** the settings section adds, shows
  records for, verifies and removes a domain.
- **Host routing** is tested at HTTP level in Go, because the browser suite
  runs against the development server with a mocked API and has no host
  routing. A real browser checks a custom hostname once against the deployed
  instance: a public page renders and the login page is refused.
- **Contract:** OpenAPI drift test and SDK generation gates.

## Done means

All of the above pass in CI, the change is merged and deployed, and a test
hostname on the production instance serves a public page and refuses the
login page.

## Left to the builder

Migration number, package placement, cache implementation, exact allowlist
path patterns, UI layout, endpoint paths and names, runbook location.
