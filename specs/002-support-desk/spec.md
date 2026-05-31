# Feature Specification: Native Support Desk

**Feature Branch**: `002-support-desk`

**Created**: 2026-05-31

**Status**: Draft

**Input**: User description: "Native support desk: per-business inbound email turned into threaded tickets, agent replies that keep the conversation threaded, triage (status/priority/tags/assignee), and bring-your-own support address/domain — scoped, isolated, and audited on the tenant foundation. No AI and no external-system sync in this slice."

## Overview

This feature delivers the first end-to-end vertical on top of the tenant foundation (spec 001): a usable, human-operated support desk. A business can receive customer email at a support address, see each conversation as a threaded ticket, triage it (assign, prioritize, tag, move through a lifecycle), and reply so the conversation stays threaded for the customer. It is the first slice in the program roadmap (`docs/ROADMAP.md`) and deliberately establishes the thin first cut of three shared platform layers — eventing/activity (SL-C), notifications (SL-D), and attachment storage (SL-E) — that later verticals reuse.

It delivers no AI and no external-system integration: AI triage/drafting is spec 003 (Agent Runtime), and Jira/Zendesk sync is spec 004 (External Ticketing Connectors). A requester (the external person emailing in) is modeled minimally here with a forward seam so the Lite CRM (spec 005) can later promote and link them to a contact. Everything inherits the foundation's tenant isolation, RBAC, no-existence-oracle, and append-only audit guarantees.

## Clarifications

### Session 2026-05-31

- Q: How does inbound email arrive? → A: A pluggable inbound-source interface with **two adapters shipping in this slice** — a provider **webhook** adapter and a **built-in SMTP receiver** (a component of the single binary on a configurable address). Connecting an existing mailbox over IMAP/OAuth is a later adapter, out of scope here. Both adapters route by recipient address through one ingestion path.
- Q: How do inbound addresses map to a business? → A: **System address + custom domains.** Each business is auto-provisioned a working, zero-config system address on a platform-hosted domain; a business may *also* verify a custom address/domain. Custom identities support three modes that never require taking over the domain's primary mail: **forward-in** (a forwarding rule, zero DNS change), **subdomain-MX** (point only a support subdomain's MX at the platform), and **provider-route** (a provider inbound route scoped to one address). Ownership is proven with a verification challenge; receiving and outbound sending identity are tracked independently.
- Q: How is the external sender (customer) modeled? → A: A **minimal, tenant-scoped requester** (email, optional display name, first/last seen), deduped by email within the tenant, referenced by tickets — with a nullable seam to link to a CRM contact later (spec 005). The requester is never a platform principal/account.
- Q: Does this slice need queues/teams? → A: **No.** Tickets are business-scoped with an optional assignee (a member principal), status, priority, and tags. Queue/team objects are a later enhancement.
- Q: Spam and message authentication? → A: **Minimal in v1.** Record SPF/DKIM/DMARC results on inbound messages and flag failures, but do not hard-reject; full spam filtering is a later enhancement. A loop/auto-responder guard is in scope to prevent mail storms.
- **Program-level decisions adopted** (from `docs/ROADMAP.md`): build order is **vertical slices, support track first**; integration model is **native system-of-record + connectors (hybrid)**; the support desk is the canonical native record that spec 004 later syncs with Jira/Zendesk.
- **Deferred to the plan** (HOW, not WHAT): concrete table design and `tenant_root_id`/`business_id` scoping for the new entities; the privileged, audited, business-scoped ingestion path (a `SECURITY DEFINER`-style function analogous to spec 001's invitation acceptance, since inbound mail carries no principal); RLS policies for the new tables; threading-token format; DKIM key generation/storage and SPF guidance; blob-storage backend (local FS + S3-compatible) and MIME-sniff allowlist; the transactional-outbox worker and notification templates. Captured in `plan-inputs.md`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Receive customer email as a threaded ticket (Priority: P1)

A customer emails a business's support address. The message appears in that business's support desk as a ticket, with the subject, body, attachments, and the sender identified as a requester. A follow-up email from the same customer about the same thread appends to the existing ticket rather than creating a new one.

**Why this priority**: Receiving and organizing inbound email into conversations is the irreducible core of a support desk; nothing else matters if mail does not reliably become a ticket.

**Independent Test**: Send an email to a business's auto-provisioned system address; confirm a ticket is created in that business with the message, subject, and a requester deduped by sender email. Send a reply referencing the first message; confirm it threads onto the same ticket. Re-deliver the same message; confirm no duplicate is created.

**Acceptance Scenarios**:

1. **Given** a business with a support address, **When** a customer sends an email to it, **Then** a ticket is created in that business containing the message, its subject, and a requester record for the sender, visible to authorized members.
2. **Given** an existing ticket, **When** the customer replies with standard threading signals (In-Reply-To/References) or the system reply token, **Then** the new message is appended to that ticket rather than starting a new one.
3. **Given** an email addressed to an address that does not resolve to any business, **When** it is received, **Then** no ticket or requester is created and no signal distinguishes "unknown address" from "exists but not yours."
4. **Given** a message that has already been ingested (same message identifier), **When** it is delivered again, **Then** the system creates no duplicate ticket or message.

---

### User Story 2 - Reply to a ticket and keep the conversation threaded (Priority: P1)

An authorized member opens a ticket and replies. The reply is delivered to the requester as an email that continues the thread, and is recorded on the ticket as an outbound message. When the requester replies again, it threads back onto the same ticket.

**Why this priority**: A support desk that can only receive is half a product. Replying with correct threading is what makes it a two-way conversation customers can follow in their own inbox.

**Independent Test**: From a ticket, send a reply; confirm an outbound message is recorded and an email is dispatched with threading headers referencing the conversation. Simulate the requester's reply (with In-Reply-To set to the outbound message); confirm it appends to the same ticket.

**Acceptance Scenarios**:

1. **Given** a ticket and a member with reply permission, **When** they send a reply, **Then** the requester receives an email that continues the thread and the reply is recorded as an outbound message on the ticket.
2. **Given** a reply was sent, **When** the requester responds to it, **Then** their response threads onto the same ticket.
3. **Given** a member with reply permission, **When** they add an internal note instead of a reply, **Then** the note is recorded on the ticket and is never delivered to the requester.
4. **Given** an outbound reply that hard-bounces, **When** the bounce is processed, **Then** the recipient is suppressed for future sends and the failure is visible on the ticket.

---

### User Story 3 - Triage a ticket (status, priority, tags, assignment) (Priority: P2)

An authorized member triages a ticket: assigns it to a teammate, sets its priority, applies tags, and moves it through its lifecycle (new → open → pending → solved → closed). A new inbound reply on a solved or closed ticket reopens it.

**Why this priority**: Triage turns a stream of messages into managed work. It builds on the conversation mechanics of P1 and is essential for a team, but the desk is already usable for a solo founder without it.

**Independent Test**: On an existing ticket, change status, priority, assignee, and tags; confirm each change persists and is audited. Then deliver an inbound reply to a solved ticket; confirm it reopens.

**Acceptance Scenarios**:

1. **Given** a ticket and a member with the appropriate permission, **When** they set status, priority, tags, or assignee, **Then** the change is persisted, takes effect immediately, and is recorded in the audit trail.
2. **Given** a ticket assigned to a member, **When** the assignment is set, **Then** the assignee must be a principal who is a member of the ticket's business or an authorized ancestor; an ineligible assignee is refused.
3. **Given** a solved or closed ticket, **When** the requester sends a new reply, **Then** the ticket reopens and the message is appended.
4. **Given** a member without triage permission, **When** they attempt to change a ticket's status or assignee, **Then** the action is refused.

---

### User Story 4 - Bring your own support address or domain (Priority: P2)

An authorized member configures the business to receive at, and reply from, the business's own address (e.g., `support@acme.com`) — without rerouting the domain's other mail. They choose forward-in, subdomain-MX, or provider-route, prove ownership via a verification challenge, and once verified, inbound routes to the business and replies are sent from the custom identity.

**Why this priority**: Customer-facing email under the business's own brand is important for a real support operation, but the desk works out of the box on the system address without it.

**Independent Test**: Add a custom domain in each supported mode; complete the verification challenge; route an inbound message to it and confirm it reaches the right business; send a reply and confirm it is sent from the custom identity. Confirm that configuring support mail does not require changing the domain's primary (whole-domain) mail flow.

**Acceptance Scenarios**:

1. **Given** a member with inbox-management permission, **When** they add a custom address/domain and complete verification, **Then** inbound mail to it routes to the business and replies can be sent from it.
2. **Given** an unverified custom domain, **When** mail is sent to it or a reply is attempted from it, **Then** it does not route and the reply is sent from the always-available system address, and the unverified state is visible.
3. **Given** a verified sending identity, **When** a reply is sent, **Then** it is authenticated for that domain (DKIM/SPF) so it is deliverable as the business's own brand.
4. **Given** a business with only the system address, **When** a customer emails it, **Then** the desk works fully without any custom-domain setup.

---

### User Story 5 - Scoped, isolated, audited support (Priority: P3)

Tickets, messages, requesters, and addresses belong to a business and its tenant. Only members authorized for that business (directly or by inheritance) can see or act on them; unrelated tenants have zero visibility; requester data never leaks across tenants; and every change is recorded in the audit trail.

**Why this priority**: Isolation and auditability are inherited guarantees from the foundation, but the new entities must be proven to uphold them. This is specified and tested explicitly rather than assumed.

**Independent Test**: Create two unrelated tenants each with a support desk; confirm neither can list, open, or reference the other's tickets, messages, or requesters by any identifier. Confirm a member lacking ticket-read permission cannot view tickets, and that each triage and reply action produced an audit entry.

**Acceptance Scenarios**:

1. **Given** two unrelated tenants, **When** a member of one attempts to access the other's tickets, messages, or requesters by any identifier, **Then** the system responds as though they do not exist (no allowed-vs-exists oracle).
2. **Given** a member authorized for a parent business, **When** they view support, **Then** they see tickets for that business and its descendants, and never those of unrelated or ancestor businesses.
3. **Given** a member without the ticket-read permission, **When** they attempt to view a ticket, **Then** the action is refused.
4. **Given** any support mutation (ingestion, reply, note, status/priority/tag/assignee change, address/domain configuration), **When** it occurs, **Then** an append-only audit entry is written in the same transaction capturing the actor (or the ingestion source), action, target, and before/after values.

---

### Edge Cases

- **Unknown / unroutable recipient**: Mail to an address that resolves to no business creates no data and is dropped; the response is indistinguishable from a routable address (no existence oracle).
- **Duplicate delivery**: The same message identifier delivered more than once never creates a duplicate ticket or message.
- **Reopen on reply**: An inbound reply to a solved/closed ticket reopens it and appends the message.
- **Spoofed attachment type**: An attachment whose declared content type does not match its sniffed bytes, or which falls outside the allowlist, is rejected; oversized attachments and oversized messages are refused.
- **Unverified custom identity**: Inbound to an unverified custom address does not route; outbound from an unverified identity falls back to the system address or is refused.
- **Mail loops / auto-responders**: Auto-reply and loop conditions are detected and suppressed so two systems cannot ping-pong tickets and replies indefinitely.
- **Same requester across businesses**: A person who emails two businesses in the same tenant is deduped to one requester within the tenant but holds separate tickets per business; across tenants, requesters are never shared.
- **Bounces**: A hard-bouncing outbound reply suppresses the recipient and surfaces the failure on the ticket.
- **Message authentication failures**: SPF/DKIM/DMARC failures are recorded and flagged but not hard-rejected in this slice.
- **Threading without signals**: When standard threading headers are absent, the system falls back to a reply token and subject match; an unmatchable message starts a new ticket rather than mis-threading.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: System MUST provision a unique, working system inbound address for a business (on a platform-hosted domain) so the business can receive support email with zero configuration.
- **FR-002**: System MUST ingest inbound email from more than one source behind a single ingestion path — a provider webhook adapter and a built-in SMTP receiver in this slice — with the ingestion mechanism pluggable so additional sources can be added without changing ticketing logic.
- **FR-003**: System MUST resolve each inbound message to exactly one business by its recipient address. Mail to an address that resolves to no business MUST be dropped without creating any data and without disclosing whether the address exists (no existence oracle).
- **FR-004**: System MUST create a ticket for a new conversation and append subsequent messages to the existing ticket, threading by standard email signals (Message-ID / In-Reply-To / References) with a system-issued reply token and a subject match as fallbacks; an unmatchable message MUST start a new ticket rather than mis-thread.
- **FR-005**: Inbound ingestion MUST be idempotent — re-delivery of the same message (by message identifier within the tenant) MUST NOT create duplicate tickets or messages.
- **FR-006**: System MUST model the external sender as a tenant-scoped requester (email, optional display name, first/last seen), deduped by email within the tenant and referenced by its tickets, with a nullable link reserved for a future CRM contact. A requester MUST NOT be a platform principal/account and MUST NOT be shared across tenants.
- **FR-007**: System MUST store message attachments with the content type determined by sniffing the file bytes (not the declared header), restricted to an explicit allowlist, and subject to a maximum size; messages exceeding a maximum size MUST be refused.
- **FR-008**: System MUST allow principals with reply permission to send a reply on a ticket; the reply MUST be delivered to the requester with threading headers that continue the conversation and MUST be recorded as an outbound message on the ticket. The requester's response MUST thread back onto the same ticket.
- **FR-009**: System MUST support an internal note on a ticket (recorded, attributed, and audited) that is never delivered to the requester.
- **FR-010**: System MUST support a ticket lifecycle (new, open, pending, solved, closed) with defined transitions; an inbound reply on a solved or closed ticket MUST reopen it.
- **FR-011**: System MUST allow principals with the appropriate permission to set a ticket's priority and tags and to assign it to a member principal; an assignee MUST be a principal that is a member of the ticket's business or an authorized ancestor, and an ineligible assignee MUST be refused. Changes MUST take effect immediately.
- **FR-012**: System MUST allow principals with inbox-management permission to configure a custom receiving/sending identity for a business in one of three modes — forward-in, subdomain-MX, or provider-route — none of which requires rerouting the domain's primary mail. Ownership MUST be proven via a verification challenge before the identity is used.
- **FR-013**: System MUST route inbound mail and send outbound replies from a custom identity only after it is verified; outbound from an unverified or absent identity MUST fall back to the always-available system address, and verified outbound MUST be domain-authenticated (DKIM/SPF) for deliverability. Hard-bouncing recipients MUST be suppressed from future sends.
- **FR-014**: System MUST write an append-only audit entry, in the same transaction as the change, for every support mutation — ingestion, reply, note, status/priority/tag/assignee change, and address/domain configuration — capturing the actor (or the ingestion source), action, target, and before/after values.
- **FR-015**: Tickets, messages, requesters, and addresses MUST be tenant-owned and scoped to the business subtree the principal is authorized for, fully isolated across tenants, with an unauthorized or unknown resource indistinguishable from one that does not exist (no allowed-vs-exists oracle), upholding the foundation's isolation guarantees.
- **FR-016**: System MUST contribute the support permissions to the platform catalog (read tickets, reply, edit/triage tickets, assign tickets, delete/redact tickets, and manage the inbox/addresses) and add them to the built-in role presets; every support action MUST be authorized against the acting principal's effective permission set, uniformly for human and non-human principals.
- **FR-017**: Inbound ingestion MUST run as a controlled, audited, business-scoped operation that carries no end-user principal and MUST NOT widen access beyond the single resolved business.
- **FR-018**: System MUST detect and suppress mail loops and auto-responders so that automated replies between two systems cannot generate an unbounded stream of tickets or outbound mail.
- **FR-019**: System MUST record message-authentication results (SPF/DKIM/DMARC) on inbound messages and flag failures, without hard-rejecting on failure in this slice.
- **FR-020**: All support list endpoints MUST be paginated with an enforced maximum page size; the inbound webhook endpoint MUST authenticate its caller (provider signature) and cap body size; inbound ingestion and outbound send MUST be rate-limited to bound abuse.

### Key Entities

- **Inbound Address**: A recipient address that routes inbound mail to a specific business. Either a system address (auto-provisioned on a platform-hosted domain) or a custom address tied to a verified domain. The routing table from "who the mail was sent to" to "which business owns the ticket."
- **Email Domain / Sending Identity**: A custom domain or address a business has configured, with its mode (forward-in, subdomain-MX, provider-route), its verification state, and its outbound authentication (DKIM/SPF) used to send replies as the business's own brand. Receiving and sending identity are tracked independently.
- **Requester**: The external person who emails in. Tenant-scoped, identified by email, deduped within the tenant, with optional display name and first/last-seen timestamps and a reserved link to a future CRM contact. Not a platform principal.
- **Ticket**: A support conversation belonging to a business. Has a subject, a requester, a status (new/open/pending/solved/closed), a priority, optional tags, and an optional assignee (a member principal). The unit of triage and the native record that spec 004 later syncs with external systems.
- **Ticket Message**: One entry in a ticket's thread — inbound (from the requester), outbound (a reply to the requester), or an internal note. Carries the message identifier and threading references used for idempotent threading, the body, and any attachments.
- **Attachment**: A file on a ticket message, stored in object storage with a sniffed content type within an allowlist and a size cap.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An email sent to a business's support address appears as a threaded, requester-identified ticket visible to authorized members within 30 seconds of receipt — 100% of well-formed messages.
- **SC-002**: Re-delivery of an already-ingested message produces zero duplicate tickets or messages — verified 100% in idempotency tests.
- **SC-003**: A requester reply that carries standard threading signals appends to the original ticket 100% of the time; and across all inbound mail, a message is never attached to the wrong ticket (0% mis-threading) — an unmatchable message opens a new ticket instead.
- **SC-004**: Tickets, messages, and requesters exhibit 0% cross-tenant visibility — no record belonging to one tenant is ever surfaced to another through any path.
- **SC-005**: 100% of support mutations (ingestion, reply, note, triage change, address/domain configuration) produce a corresponding append-only audit entry.
- **SC-006**: Mail to an unknown or unauthorized address creates no data and returns a response indistinguishable from a routable address — verified 100% in no-oracle tests.
- **SC-007**: Attachments whose sniffed type falls outside the allowlist, or which exceed the size cap, are rejected 100% of the time; declared content type is never trusted.
- **SC-008**: A custom domain can be configured and verified, after which inbound routes to the correct business and replies send as that domain — while the domain's primary (whole-domain) mail flow remains unchanged — demonstrated end-to-end for each of the three modes.
- **SC-009**: A member lacking the ticket-read permission cannot view any ticket, and each support permission grants exactly the actions in its set and denies all others — verified across 100% of permission-enforcement tests.
- **SC-010**: At 10,000 tickets per business and realistic thread depth, ticket-list and ticket-load operations complete within a p95 latency target of 200 ms, enforced by automated performance tests.
- **SC-011**: A mail loop between two automated systems is detected and suppressed before it produces more than a bounded number of tickets/replies — verified by an automated loop test.

## Assumptions

- **Email-only channel**: This slice handles email. Web-form, API, chat, and other channels are later enhancements.
- **Internal agent experience only**: The UI is the internal agent-facing view (extending the existing dashboard). A customer-facing portal is out of scope here (portal-style submission appears in spec 006, Feedback).
- **No AI in this slice**: Triage suggestions, reply drafting, and summarization are delivered by spec 003 (Agent Runtime) acting on these tickets.
- **No external-system sync**: Bidirectional sync with Jira/Zendesk and connector-driven tooling are spec 004; the support desk is the native system of record those connectors later mirror.
- **Requester is not a principal**: The external sender is a minimal tenant-scoped entity with a reserved CRM-contact link; spec 005 promotes/links requesters to contacts and may associate them with businesses.
- **No queues/teams**: Routing is by individual assignee plus tags; queue/team objects are a later enhancement.
- **Minimal spam handling**: Message-authentication results are recorded and flagged but not used to hard-reject in v1; full spam filtering is deferred.
- **Single binary**: The built-in SMTP receiver is a component of the one deployable binary on a configurable address (per the constitution's modular-monolith rule), alongside the provider webhook adapter for hosted email providers.
- **Outbound email capability exists**: This slice extends the foundation's outbound mailer (spec 001) with templated, threaded, domain-authenticated sending and bounce suppression.
- **Inherited foundation guarantees**: Tenant isolation (dual-enforced), RBAC against effective permissions, the no-existence-oracle boundary, append-only auditing, cursor pagination, and rate limiting are provided by spec 001 and are extended — not redefined — here.
- **Billing and usage metering are out of scope** for this feature.
