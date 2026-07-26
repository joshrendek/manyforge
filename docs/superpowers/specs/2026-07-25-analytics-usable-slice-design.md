# Design — Analytics usable slice (manyforge-as0, first cut)

**Status:** in progress · **Date:** 2026-07-25 · **Epic:** `manyforge-as0` · **Builds on:** `manyforge-p20`

## Goal

Get from "p20 plumbing exists" to **an operator can drop a `<script>` on a site and watch real
traffic in a dashboard**. Everything here is what stands between those two states.

## What p20 already gives us

Partitioned `analytics_event` (keyed on `ingested_at`, 90-day retention), the `mfk_` publishable
key model, a principal-less ingest path with no-oracle 401s, an idempotent watermark rollup worker,
and partition lifecycle management. None of that is rebuilt.

## What's missing (this spec)

1. Pageview-shaped storage — `path`, `referrer_host`, `visitor_hash`, `is_bot`.
2. Cookieless unique-visitor counting.
3. An embeddable snippet, served from the API.
4. A lightweight collect endpoint the snippet can call.
5. Rollups a dashboard can actually read: visitors, pageviews, top pages, top referrers.
6. Two UI screens: site/key management, and the dashboard itself.

## 1. Pageview columns

`ALTER TABLE analytics_event ADD COLUMN` on the partitioned parent propagates to every partition.

| Column | Why typed rather than in `props` |
|---|---|
| `path text` | Grouped and indexed for "top pages" |
| `referrer_host text` | Host only — the full referrer URL is a privacy liability and useless for aggregates |
| `visitor_hash bytea` | Counted distinctly per day; needs an index |
| `is_bot boolean` | Filtered out of every aggregate, but kept so the filter can be audited |

`name` stays (`'pageview'` for now) so custom events remain possible later without another migration.

## 2. Cookieless visitor counting — the privacy-critical part

No cookies, no persistent identifier, no cross-site profile.

```
visitor_hash = sha256(daily_salt ‖ client_id ‖ ip ‖ user_agent)   -- truncated to 16 bytes
```

- The salt lives in `analytics_salt(day, salt)` and is generated server-side with `crypto/rand`.
- **Raw IP and User-Agent are hashed in the same statement that inserts, and never stored.** No
  column holds them; no log line prints them.
- Salts older than the retention window are deleted, so an old `visitor_hash` cannot be re-derived
  even by someone holding the whole database. This is what makes the hash non-reversible in
  practice rather than only in theory — a fixed salt would let anyone with the DB and a candidate
  IP list confirm visitors.
- The hash rotates daily, so "unique visitors" is inherently a per-day measure and no identifier
  survives midnight. Cross-day user tracking is *impossible by construction*, not by policy.

Bot filtering is a substring match over a small UA denylist. Cheap, and it keeps obvious crawler
noise out of the numbers.

## 3. The snippet

Served at `GET /a.js` — public, no auth, cached. Reads its own `data-key` attribute:

```html
<script defer src="https://hub.bluescripts.net/a.js" data-key="mfk_..."></script>
```

Sends a pageview on load, and on SPA navigation by wrapping `history.pushState`/`replaceState` and
listening for `popstate`. Uses `navigator.sendBeacon` when available so a pageview isn't lost when
the tab closes. Sends only `{k, p, r}` — key, path, referrer host. No IDs, no fingerprinting.

Respects `navigator.doNotTrack` and skips `localhost`/file origins.

**No Subresource Integrity on the embed tag, deliberately.** SRI pins a hash of the script, so
every snippet update would silently break every site that embedded the old hash — the embed is
meant to be a paste-once-and-forget tag. This matches Plausible, Fathom, and GA. The tradeoff is
real and worth stating: a compromise of the hub origin would let attacker JS run on every embedding
site. That risk is carried by origin security (HTTPS, restricted deploy path), not by SRI. If a
tenant wants SRI they can self-host the snippet and pin it themselves, since it is dependency-free
and static.

## 4. Collect endpoint

`POST /a/e` — principal-less, on the same ingress group as the other public endpoints, behind the
per-IP limiter.

Deliberately NOT the p20 batch endpoint: the snippet should stay tiny, and a browser-supplied
`occurred_at` is worthless anyway (clock skew, and it is untrusted input we would only clamp).
`occurred_at` here is the server clock, same as `ingested_at`.

Same no-oracle rule: unknown, revoked, and malformed keys return an identical `204`. A public
collect endpoint should not confirm which keys exist — and since a beacon has no error handling
anyway, 204-always is both safer and simpler than the 401 the batch API returns.

## 5. Rollups

Three tables, all recompute-not-increment (the p20 invariant), all driven by the same watermark
worker:

- `analytics_daily(business, client, date)` → `pageviews`, `visitors` (distinct `visitor_hash`)
- `analytics_page_daily(business, client, date, path)` → `pageviews`, `visitors`
- `analytics_referrer_daily(business, client, date, referrer_host)` → `pageviews`, `visitors`

Bots are excluded at rollup time. The existing `analytics_event_daily` from p20 stays as the
generic event counter; these are the pageview-specific views a dashboard reads.

## 6. Read API

`GET /businesses/{id}/analytics/summary?client_id&from&to` — gated by `telemetry.read`, RLS-scoped
by business. Returns totals, a daily series, top pages, and top referrers, all read from the rollup
tables (never from raw events, so the dashboard stays fast as volume grows).

## 7. UI

- **Sites & keys** (`manyforge-mq4f`) — list/create/revoke clients, with the copy-paste embed
  snippet shown inline. The `require_signature` toggle must be clearly labelled server-to-server
  only; turning it on for a website would require embedding the `mfs_` secret in a page, which is
  exactly the failure PR #53 fixed at the data layer.
- **Analytics dashboard** — visitor/pageview totals, a daily chart, top pages, top referrers, with
  a site selector and date range.

## Test plan

**Unit** — visitor-hash determinism within a day and divergence across days; bot UA matching; path
normalisation (query string and fragment stripped, trailing slash); referrer host extraction;
snippet payload validation.

**Integration** — collect endpoint writes a pageview with a hash and no raw IP/UA anywhere in the
row; unknown/revoked/malformed keys all return an identical 204 and persist nothing; rollups
produce correct visitors vs pageviews (same visitor twice = 2 pageviews, 1 visitor); bots excluded;
rollup idempotency preserved; the read API is business-scoped and refuses another tenant.

**Security pins** — no column or log statement holds raw IP/UA; the salt is `crypto/rand`; old
salts are deleted; rollups exclude bots; read API is permission-gated.

**Browser** — Playwright: load a page with the snippet, assert the beacon fires, assert the
dashboard shows the pageview. Plus a real-browser pass over both new screens before calling done.


---

## Addendum — enrichment (2026-07-25, migration 0107)

Closes the epic's task 4. Adds campaign attribution, device/browser, and country.

### What is stored, and why these shapes

| Column | Derived from | Why this granularity |
|---|---|---|
| `utm_source/medium/campaign` | query string, allowlisted by name | The three keys marketing actually uses. An allowlist, never a denylist — query strings carry session tokens and email addresses, so "store it and filter later" is a leak waiting for the next parameter name. |
| `device_type` | User-Agent | Exactly three buckets. Finer classification ("iPhone 15 Pro") is what turns a device string into a fingerprint, and the question a dashboard answers is "should I care about mobile layout?". |
| `browser` | User-Agent | Coarse family. Ordered matching, because User-Agents lie by design: Edge claims Chrome, Chrome claims Safari. |
| `country` | trusted edge header, in flight | ISO alpha-2. A raw IP identifies a household; a country does not. |

The rule for anything added later: **if a value could meaningfully narrow a population to a
person, it does not belong in a column.** The raw User-Agent and IP remain hash inputs only.

### One generic dimension table

`analytics_dimension_daily(client, date, dimension, value)` rather than a table per breakdown.
Adding "operating system" or "language" later becomes a new `dimension` value plus one line in the
rollup — no migration, no new table, no new read query. `analytics_page_daily` and
`analytics_referrer_daily` predate it and keep their own tables: they already carry live data and
their own indexes, and rewriting them would be a data migration for no functional gain.

### Country is optional, deliberately

Cloudflare deployments may explicitly enable `MANYFORGE_TRUST_CF_IPCOUNTRY`, which reduces the
edge-supplied `CF-IPCountry` header to ISO alpha-2 during collection. It is disabled by default:
trusting that header is safe only when Cloudflare is the sole public path to the origin or the
ingress independently strips or overwrites client-supplied copies. Unknown and special values are
dropped. No IP-to-country database, raw IP, or finer location is stored. The dashboard says when
the signal is absent rather than showing an empty panel — an absent breakdown is honest, a guessed
one is worse than none.

### HLL was considered and rejected

The epic proposes a cardinality sketch (HLL) for unique visitors. Not implemented, on purpose:

- `count(DISTINCT visitor_hash)` runs against **one day's partition** — the hash rotates daily, so
  the distinct set is bounded by a single day's traffic, not by history. At the ~1M events/day
  design target that is a routine aggregate.
- The rollup runs once a minute over a five-minute window, not per request, so this is not on any
  hot path.
- HLL trades exactness for memory on a problem we do not have, and an approximate visitor count is
  much harder to explain to a tenant than an exact one.

Revisit if a single site's daily distinct visitors approach the tens of millions — at which point
the sketch belongs in `analytics_dimension_daily`'s rollup, not in the read path.
