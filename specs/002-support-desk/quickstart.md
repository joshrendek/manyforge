# Quickstart: Native Support Desk

How to bring up the support desk locally and exercise it end-to-end — inbound
email → threaded ticket → reply → customer reply threads back → custom domain.
Builds on the tenant foundation (spec 001); reuse its account/business flow.

## Prerequisites

- Go 1.25 (matches the module)
- PostgreSQL 16 (local or Docker)
- Docker (for integration tests — testcontainers spins ephemeral Postgres)
- Node (for the Angular `web/` agent UI + Playwright e2e)
- `make`, `sqlc`, `golang-migrate`
- `swaks` (Swiss Army Knife for SMTP — `brew install swaks`) for the SMTP
  receiver smoke; `openssl`/`nc` work as a raw fallback

## Configure

Start from the foundation's `.env` and add the support-desk variables.

```bash
cp .env.example .env            # DB DSN, JWT keypair, outbound mailer (spec 001)
```

```bash
# --- Support desk (spec 002) ---

# Built-in SMTP receiver. An in-process component of the single binary (NOT a
# second service). Empty disables it; the webhook adapter still works.
# Port <1024 needs root/CAP_NET_BIND_SERVICE — use :2525 locally.
MANYFORGE_SMTP_ADDR=:2525

# Inbound provider webhook adapter. HMAC-SHA256 signing secret used to verify
# the X-Manyforge-Signature header (constant-time). Required to accept webhooks.
MANYFORGE_INBOUND_WEBHOOK_SECRET=dev-inbound-secret-change-me

# Attachment blob backend (SL-E). Local filesystem for self-host, or an
# S3-compatible URL. The directory is created if missing.
MANYFORGE_BLOB_URL=file:///tmp/manyforge-blobs
#   S3-compatible alternative:
#   MANYFORGE_BLOB_URL=s3://manyforge-attachments?region=us-east-1&endpoint=http://localhost:9000

# Outbound mailer — reuse spec 001's. Unset logs sent mail to stdout (dev),
# which is what lets you read reply/notification email without a real MTA.
MANYFORGE_SMTP_URL=

# Platform-hosted domain that auto-provisioned system inbound addresses live on.
MANYFORGE_INBOUND_SYSTEM_DOMAIN=inbound.localhost

# DKIM signing key for verified custom sending identities (FR-013). Optional in
# dev — unset/absent means verified-outbound is unsigned locally; a real key is
# required for deliverable, domain-authenticated brand mail.
MANYFORGE_DKIM_KEY_PATH=./secrets/dkim/default.private
```

> The application DB role must stay non-superuser / non-BYPASSRLS — the six new
> support tables are RLS-enforced exactly like the foundation's. The principal-less
> ingestion path uses an audited `SECURITY DEFINER` function scoped to the one
> resolved business; it is the only controlled exception.

## Run

```bash
make migrate                    # apply forward-only migrations (adds support_desk,
                                #   RLS + ingestion fn, permissions, events/notify)
make generate                   # sqlc → generated query code (never hand-edit)
make dev                        # API on :8080 AND the SMTP listener on :2525
```

On boot you should see both listeners and the outbox worker start, e.g.:

```text
msg="http listening" addr=:8080
msg="smtp receiver listening" addr=:2525
msg="outbox worker started"
```

Health `GET /healthz` · readiness `GET /readyz` · metrics `GET /metrics`. The
new endpoints are versioned under `/api/v1`; the contract is
`specs/002-support-desk/contracts/openapi.yaml` (the OpenAPI-drift unit test and
`make contract-test` fail CI if the router and contract diverge).

```bash
# Angular agent UI (separate terminal) — adds the support/ feature area:
cd web && npm install && npm run start    # proxies /api → :8080
```

## Validation walkthrough (maps to spec acceptance scenarios)

> Run against a fresh DB. Every step is also covered by an automated test; this
> is the manual smoke path. `$API=http://localhost:8080/api/v1`.

### 1. Sign in + create a business (spec 001 flow, briefly)

```bash
# Signup → verify → login → create a top-level business (creator is Owner).
curl -s $API/auth/signup -d '{"email":"agent@acme.test","password":"S3cret!pass"}'
# consume the emailed token (printed to stdout by the dev mailer):
curl -s $API/auth/verify-email -d '{"token":"<from-stdout>"}'
TOKENS=$(curl -s $API/auth/login -d '{"email":"agent@acme.test","password":"S3cret!pass"}')
ACCESS=$(echo "$TOKENS" | jq -r .access_token)
BIZ=$(curl -s $API/businesses -H "Authorization: Bearer $ACCESS" \
        -d '{"name":"Acme Support"}' | jq -r .id)
```

### 2. Note the auto-provisioned system inbound address (FR-001)

Every business gets a working, zero-config address on the platform-hosted domain
the moment it is created.

```bash
curl -s $API/businesses/$BIZ/inbound-addresses -H "Authorization: Bearer $ACCESS" | jq
# → [{ "address": "acme-support-7f3a@inbound.localhost", "kind": "system",
#      "verified": true }]
ADDR=acme-support-7f3a@inbound.localhost     # ← use yours from the response
```

✅ Expect: exactly one `system` address, already usable. No DNS, no config.

### 3. Deliver a test inbound email — TWO ways

Both adapters route by recipient address through the one ingestion path
(FR-002). Pick either; both land the same ticket.

**(a) Provider webhook** — POST the message and sign the body with
`MANYFORGE_INBOUND_WEBHOOK_SECRET` (HMAC-SHA256, hex; verified constant-time).

```bash
SECRET=dev-inbound-secret-change-me
BODY=$(cat <<JSON
{
  "from": "Dana Customer <dana@example.com>",
  "to": "$ADDR",
  "subject": "Cannot reset my password",
  "message_id": "<msg-001@example.com>",
  "text": "Hi — the reset link 404s. Help?",
  "spf": "pass", "dkim": "pass", "dmarc": "pass"
}
JSON
)
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.* //')

curl -s $API/inbound/email/webhook \
  -H "Content-Type: application/json" \
  -H "X-Manyforge-Signature: sha256=$SIG" \
  --data-binary "$BODY"
# → 202 Accepted (the body is captured; the response never reveals routing)
```

**(b) Built-in SMTP receiver** — talk SMTP to the in-process listener with
`swaks`. The recipient (`RCPT TO`) is what routes it.

```bash
swaks --server localhost --port 2525 \
      --from dana@example.com \
      --to "$ADDR" \
      --h-Subject "Cannot reset my password" \
      --h-Message-ID "<msg-001@example.com>" \
      --body "Hi — the reset link 404s. Help?"
```

Raw fallback (no swaks) — `nc` for a plaintext listener, or `openssl s_client`
if STARTTLS is on:

```bash
printf 'EHLO local\r\nMAIL FROM:<dana@example.com>\r\nRCPT TO:<%s>\r\nDATA\r\n%s\r\n.\r\nQUIT\r\n' \
  "$ADDR" \
  $'Subject: Cannot reset my password\r\nMessage-ID: <msg-001@example.com>\r\n\r\nHi — the reset link 404s. Help?' \
  | nc localhost 2525
# STARTTLS variant: openssl s_client -starttls smtp -connect localhost:2525 -quiet
```

✅ Expect: one ticket created with the subject, body, and a requester deduped
by `dana@example.com` within the tenant (SC-001). Because both sends share
`Message-ID: <msg-001@example.com>`, the second delivery is a no-op — **no
duplicate ticket or message** (FR-005, SC-002).

### 4. GET the ticket via the API (FR-015 / no-oracle)

```bash
TICKET=$(curl -s "$API/businesses/$BIZ/tickets" -H "Authorization: Bearer $ACCESS" \
          | jq -r '.items[0].id')
curl -s $API/tickets/$TICKET -H "Authorization: Bearer $ACCESS" | jq
# → { status:"new", priority:"normal", requester:{email:"dana@example.com"},
#     messages:[{ direction:"inbound", subject:"Cannot reset my password", ... }],
#     auth_results:{ spf:"pass", dkim:"pass", dmarc:"pass" } }
```

✅ Expect: a member of an unrelated tenant gets `404` for this same `$TICKET`
(indistinguishable from "does not exist"); a member lacking `tickets.read` is
refused (SC-004, SC-009).

### 5. POST a reply → outbound message (FR-008)

```bash
curl -s $API/tickets/$TICKET/reply -H "Authorization: Bearer $ACCESS" \
  -d '{"body":"Hi Dana — fixed the link, try again and let us know."}'
# → 201; ticket now carries an outbound message.
```

The dev mailer prints the dispatched email to stdout. Confirm it threads:

```text
To: dana@example.com
From: acme-support-7f3a@inbound.localhost
Subject: Re: Cannot reset my password
Message-ID: <reply-af12@inbound.localhost>
In-Reply-To: <msg-001@example.com>
References: <msg-001@example.com>
Reply-To: acme-support-7f3a+tkt_<token>@inbound.localhost   # unforgeable reply token
```

✅ Expect: the outbound message is recorded on the ticket and audited; the
threading headers + the HMAC reply token continue the conversation. An
**internal note** (`POST /tickets/$TICKET/notes`) is recorded but never
mailed (FR-009).

### 6. Simulate a customer reply that threads back (FR-008, US2)

Send a new inbound message whose `In-Reply-To` points at the outbound
`Message-ID` from step 5 (read it off the stdout dump). It must append to the
**same** ticket, not open a new one.

```bash
swaks --server localhost --port 2525 \
      --from dana@example.com --to "$ADDR" \
      --h-Subject "Re: Cannot reset my password" \
      --h-Message-ID "<msg-002@example.com>" \
      --h-In-Reply-To "<reply-af12@inbound.localhost>" \
      --body "That worked, thank you!"

curl -s $API/tickets/$TICKET -H "Authorization: Bearer $ACCESS" | jq '.messages | length'
# → 3   (inbound, outbound, inbound) — still ONE ticket
```

✅ Expect: appended to `$TICKET` (0% mis-threading, SC-003). Replying onto a
`solved`/`closed` ticket reopens it (FR-010). When headers are absent the
system falls back to the reply token then a `[#ref]` subject match; an
unmatchable message starts a new ticket rather than mis-threading.

### 7. Configure a custom domain (forward-in) + DNS TXT verification (FR-012)

Add the business's own address in `forward_in` mode (a forwarding rule — zero
change to the domain's primary mail flow), then prove ownership via a TXT
challenge.

```bash
DOMAIN=$(curl -s $API/businesses/$BIZ/email-domains -H "Authorization: Bearer $ACCESS" \
  -d '{"domain":"acme.com","mode":"forward_in","sending_address":"support@acme.com"}')
echo "$DOMAIN" | jq
# → { id:"...", mode:"forward_in", verified:false,
#     verification:{ type:"TXT", host:"_manyforge.acme.com",
#                    value:"manyforge-verify=ab12cd34..." } }
DOMAIN_ID=$(echo "$DOMAIN" | jq -r .id)
```

Publish the TXT record at your DNS provider, then trigger verification:

```text
_manyforge.acme.com.   IN   TXT   "manyforge-verify=ab12cd34..."
```

```bash
# Locally you can point the resolver at a stub, or rely on the verification
# job's poll. Kick it manually:
curl -s $API/email-domains/$DOMAIN_ID/verify -H "Authorization: Bearer $ACCESS" | jq
# → { verified:true }   once the TXT resolves
```

✅ Expect: once verified, inbound to `support@acme.com` routes to `$BIZ` and
replies send from the custom identity (DKIM-signed when `MANYFORGE_DKIM_KEY_PATH`
is set). While **unverified**, inbound does not route and outbound falls back to
the always-available system address (FR-013, SC-008). The domain's primary
(whole-domain) mail flow is never touched.

## Test (the merge gate — Constitution Principle III)

```bash
make test           # unit tests (fast, no DB): source-level security pins + OpenAPI drift
make int-test       # ALL integration tests (testcontainers ephemeral Postgres; Docker required)
make sec-test       # internal/security_regression: support isolation, ingestion scope,
                    #   threading idempotency, MIME-sniff, webhook signature, no-oracle
make contract-test  # NEW — shared-layer interface contracts (InboundSource, Blob,
                    #   Notifier, event bus) + the support OpenAPI contract
cd web && npm run e2e   # Playwright support flow: inbound email → ticket → reply → outbound
```

CI runs `make test && make int-test && make contract-test && make lint`
(`int-test` ⊇ `sec-test`); all green required to merge.

### Performance check (SC-010)

`go test -tags integration ./internal/ticketing -run TestSC010 -count=1` seeds
10,000 tickets/business at realistic thread depth and asserts ticket-list and
ticket-load p95 < 200 ms (RLS enabled).

## Troubleshooting

- **My inbound email vanished.** A recipient that resolves to no business is
  **silently dropped** by design — no ticket, no requester, and the response is
  indistinguishable from a routable address (FR-003, SC-006). This is expected;
  double-check `$ADDR` matches the system address from step 2.
- **Webhook returns 401/403.** The HMAC didn't verify. Confirm the curl is
  signing the **exact bytes** sent (use `--data-binary`, not `-d`, which can
  reformat), that `SECRET` matches `MANYFORGE_INBOUND_WEBHOOK_SECRET`, and that
  the header is `sha256=<hex>`.
- **SMTP connection refused / permission denied on bind.** Ports below 1024
  need root or `CAP_NET_BIND_SERVICE`; use `MANYFORGE_SMTP_ADDR=:2525` locally.
  If the listener didn't start, `MANYFORGE_SMTP_ADDR` is probably empty (the
  webhook adapter still works on its own).
- **Attachment rejected.** Content type is decided by **sniffing the bytes**,
  not the declared header; types outside the allowlist or over the size cap are
  refused (FR-007, SC-007). Oversized whole messages are refused too.
- **Reply not threading.** Make sure the simulated customer reply sets
  `In-Reply-To` to the outbound `Message-ID` (or carries the `support+tkt_…`
  reply token); without either, it falls back to subject match and may open a
  new ticket — that's the anti-mis-threading guard, not a bug.
- **Blob errors.** For `file:///…` ensure the path is writable; for `s3://…`
  ensure the endpoint/region/credentials are reachable from the process.
