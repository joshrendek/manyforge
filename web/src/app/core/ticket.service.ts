import { HttpClient, HttpParams } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

// Typed client for the support-desk (US1 read) API. Thin by design — mirrors
// business.service.ts: hard-coded /api/v1 URLs, typed Observables, no auth
// handling (the global authInterceptor attaches the bearer + retries on 401).

// ── Enums (openapi.yaml:324-349) ───────────────────────────────────────────
export type TicketStatus = 'new' | 'open' | 'pending' | 'solved' | 'closed';
export type TicketPriority = 'low' | 'normal' | 'high' | 'urgent';
export type MessageDirection = 'inbound' | 'outbound' | 'note';
// Used for spf/dkim/dmarc result fields.
export type DnsRecordState = 'unknown' | 'pending' | 'pass' | 'fail';

// ── Component schemas (openapi.yaml:352-404) ───────────────────────────────

// Requester: tenant-scoped external sender, deduped by email within the tenant;
// never a principal (FR-006).
export interface Requester {
  id: string;
  tenant_root_id: string;
  email: string;
  display_name: string | null;
  // Reserved CRM-contact seam (spec 005); always null in this slice.
  contact_id: string | null;
  first_seen_at: string;
  last_seen_at: string;
}

export interface Attachment {
  id: string;
  filename: string;
  // Sniffed MIME type (allowlisted); the declared header is never trusted (FR-007).
  content_type: string;
  size: number;
  // Tenant-scoped object-storage key.
  blob_key: string;
}

// Ticket embeds the full Requester inline. Does NOT expose reply_token (DB-only).
export interface Ticket {
  id: string;
  business_id: string;
  tenant_root_id: string;
  subject: string;
  status: TicketStatus;
  priority: TicketPriority;
  // Member principal of this business or an authorized ancestor (FR-011);
  // null = unassigned.
  assignee_principal_id: string | null;
  requester: Requester;
  tags: string[];
  message_count: number;
  last_message_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface TicketMessage {
  id: string;
  ticket_id: string;
  direction: MessageDirection;
  // RFC822 Message-ID; basis of idempotent threading (FR-004/FR-005).
  message_id: string | null;
  // RFC822 In-Reply-To header.
  in_reply_to: string | null;
  // RFC822 References chain.
  references: string[];
  // Member who authored an outbound reply or note; null for inbound.
  author_principal_id: string | null;
  body_text: string | null;
  body_html: string | null;
  attachments: Attachment[];
  spf_result: DnsRecordState;
  dkim_result: DnsRecordState;
  dmarc_result: DnsRecordState;
  // Outbound delivery state (pending|sent|failed); null for inbound/note (US2).
  delivery_state?: 'pending' | 'sent' | 'failed' | null;
  created_at: string;
}

// Keyset page envelope (openapi.yaml:319-321): { items, next_cursor }.
export interface Page<T> {
  items: T[];
  next_cursor: string | null;
}

// A human, active member principal eligible to be a ticket assignee (FR-011),
// returned by GET /businesses/{id}/assignable-members to power the picker. `id` is
// the principal id used for ticket.assignee_principal_id.
export interface AssignableMember {
  id: string;
  email: string;
  display_name: string;
}

// Optional list filters for GET /businesses/{id}/tickets. `assignee` accepts the
// literal `unassigned` to list tickets with no assignee.
export interface TicketListFilters {
  status?: TicketStatus;
  priority?: TicketPriority;
  assignee?: string;
  tag?: string;
  cursor?: string;
  limit?: number;
}

// Partial triage update (openapi.yaml:531-540). Every field is optional; only
// fields PRESENT in the body change, omitted fields keep their current value
// (constitution partial-update rule). Tri-state on the assignee:
//   - key absent  → leave the assignee unchanged
//   - key === null → unassign
//   - key === uuid → assign (backend validates eligibility / tickets.assign)
// `tags`, when present, is a FULL replacement of the tag set ([] clears it).
export interface PatchTicket {
  status?: TicketStatus;
  priority?: TicketPriority;
  tags?: string[];
  assignee_principal_id?: string | null;
}

// ── US4 inbox-management views (openapi.yaml EmailDomain/InboundAddress) ────
// Field names match internal/ticketing/identity.go json tags EXACTLY (snake_case).

// EmailDomainMode: how inbound mail reaches us; none reroutes the domain's
// primary mail. EmailDomainVerification: the derived ownership-proof state.
export type EmailDomainMode = 'forward_in' | 'subdomain_mx' | 'provider_route';
export type EmailDomainVerification = 'unverified' | 'pending' | 'verified' | 'failed';

// A single publishable DNS record {name, value}.
export interface DnsTxtRecord {
  name: string;
  value: string;
}

// The records an operator publishes to prove ownership and enable outbound auth
// (FR-012/FR-013). mx_hint is only set for mode=subdomain_mx; null otherwise.
export interface DnsChallenge {
  verification_txt: DnsTxtRecord;
  dkim_record: DnsTxtRecord;
  spf_hint: string;
  mx_hint: string | null;
}

// EmailDomain is the API view of a custom domain / sending identity. verified_at
// is null until verified; dns_challenge is recomposed on every read so the
// operator can re-fetch the records to publish.
export interface EmailDomain {
  id: string;
  business_id: string;
  tenant_root_id: string;
  domain: string;
  mode: EmailDomainMode;
  verification: EmailDomainVerification;
  verified_at: string | null;
  dkim_state: DnsRecordState;
  spf_state: DnsRecordState;
  dns_challenge: DnsChallenge;
  created_at: string;
}

// InboundAddress is a routing entry: the auto-provisioned system address plus any
// custom addresses bound to a verified email domain. email_domain_id is null for
// kind=system.
export interface InboundAddress {
  id: string;
  business_id: string;
  tenant_root_id: string;
  address: string;
  kind: 'system' | 'custom';
  email_domain_id: string | null;
  active: boolean;
  created_at: string;
}

@Injectable({ providedIn: 'root' })
export class TicketService {
  private http = inject(HttpClient);

  // GET /businesses/{id}/tickets — keyset-paginated, optional status/priority/
  // assignee/tag filters (requires tickets.read). 404 for unknown OR unauthorized.
  listTickets(businessId: string, filters: TicketListFilters = {}): Observable<Page<Ticket>> {
    let params = new HttpParams();
    if (filters.status) params = params.set('status', filters.status);
    if (filters.priority) params = params.set('priority', filters.priority);
    if (filters.assignee) params = params.set('assignee', filters.assignee);
    if (filters.tag) params = params.set('tag', filters.tag);
    if (filters.cursor) params = params.set('cursor', filters.cursor);
    if (filters.limit != null) params = params.set('limit', String(filters.limit));
    return this.http.get<Page<Ticket>>(`/api/v1/businesses/${businessId}/tickets`, { params });
  }

  // GET /businesses/{id}/tickets/{tid} — single ticket (requires tickets.read).
  getTicket(businessId: string, ticketId: string): Observable<Ticket> {
    return this.http.get<Ticket>(`/api/v1/businesses/${businessId}/tickets/${ticketId}`);
  }

  // PATCH /businesses/{id}/tickets/{tid} — partial triage update (US3). Returns
  // the updated Ticket (200). Requires tickets.write; assignment additionally
  // requires tickets.assign. The caller is responsible for the assignee
  // tri-state: omit `assignee_principal_id` to leave it unchanged, pass literal
  // `null` to unassign, or a uuid to assign. We forward `patch` verbatim so an
  // omitted key stays omitted on the wire.
  patchTicket(businessId: string, ticketId: string, patch: PatchTicket): Observable<Ticket> {
    return this.http.patch<Ticket>(`/api/v1/businesses/${businessId}/tickets/${ticketId}`, patch);
  }

  // GET /businesses/{id}/assignable-members — the business's human members eligible
  // to be a ticket assignee (FR-011), for the assignee picker. Single server-capped
  // page; requires tickets.assign (404 when the caller cannot assign).
  listAssignableMembers(businessId: string): Observable<Page<AssignableMember>> {
    return this.http.get<Page<AssignableMember>>(
      `/api/v1/businesses/${businessId}/assignable-members`,
    );
  }

  // GET /businesses/{id}/tickets/{tid}/messages — keyset-paginated thread of
  // inbound + outbound + note messages, in order (requires tickets.read).
  listMessages(
    businessId: string,
    ticketId: string,
    cursor?: string,
    limit?: number,
  ): Observable<Page<TicketMessage>> {
    let params = new HttpParams();
    if (cursor) params = params.set('cursor', cursor);
    if (limit != null) params = params.set('limit', String(limit));
    return this.http.get<Page<TicketMessage>>(
      `/api/v1/businesses/${businessId}/tickets/${ticketId}/messages`,
      { params },
    );
  }

  // GET /businesses/{id}/requesters — keyset-paginated, optional exact
  // (case-insensitive) email filter for lookup/dedup (requires tickets.read).
  listRequesters(
    businessId: string,
    email?: string,
    cursor?: string,
    limit?: number,
  ): Observable<Page<Requester>> {
    let params = new HttpParams();
    if (email) params = params.set('email', email);
    if (cursor) params = params.set('cursor', cursor);
    if (limit != null) params = params.set('limit', String(limit));
    return this.http.get<Page<Requester>>(`/api/v1/businesses/${businessId}/requesters`, {
      params,
    });
  }

  // GET /businesses/{id}/requesters/{rid} — single requester (requires tickets.read).
  getRequester(businessId: string, requesterId: string): Observable<Requester> {
    return this.http.get<Requester>(`/api/v1/businesses/${businessId}/requesters/${requesterId}`);
  }

  // POST /businesses/{bid}/tickets/{tid}/reply — send an outbound reply (US2).
  // Returns the created TicketMessage (201). Requires tickets.reply permission.
  reply(
    businessId: string,
    ticketId: string,
    body: { body_text: string; body_html?: string },
  ): Observable<TicketMessage> {
    return this.http.post<TicketMessage>(
      `/api/v1/businesses/${businessId}/tickets/${ticketId}/reply`,
      body,
    );
  }

  // POST /businesses/{bid}/tickets/{tid}/note — add an internal note (US2).
  // Returns the created TicketMessage (201). Requires tickets.reply permission.
  addNote(
    businessId: string,
    ticketId: string,
    body: { body_text: string },
  ): Observable<TicketMessage> {
    return this.http.post<TicketMessage>(
      `/api/v1/businesses/${businessId}/tickets/${ticketId}/note`,
      body,
    );
  }

  // ── US4 inbox management (requires inbox.manage; 404 for unknown/unauthorized) ──

  // GET /businesses/{id}/email-domains — keyset-paginated list of the business's
  // custom email domains with verification + DKIM/SPF state and their DNS challenge.
  listEmailDomains(businessId: string, cursor?: string): Observable<Page<EmailDomain>> {
    let params = new HttpParams();
    if (cursor) params = params.set('cursor', cursor);
    return this.http.get<Page<EmailDomain>>(`/api/v1/businesses/${businessId}/email-domains`, {
      params,
    });
  }

  // POST /businesses/{id}/email-domains — add a custom domain in one of three modes.
  // Returns the created EmailDomain (201) in `unverified` state with the DNS
  // challenge to publish before verifying. 400 invalid domain/mode; 409 duplicate.
  createEmailDomain(
    businessId: string,
    body: { domain: string; mode: EmailDomainMode },
  ): Observable<EmailDomain> {
    return this.http.post<EmailDomain>(`/api/v1/businesses/${businessId}/email-domains`, body);
  }

  // POST /businesses/{id}/email-domains/{did}/verify — trigger DNS verification.
  // Idempotent: returns the current EmailDomain (200) — verified, or still
  // unverified when the challenge isn't observed yet (a pending poll, NOT an error).
  verifyEmailDomain(businessId: string, domainId: string): Observable<EmailDomain> {
    return this.http.post<EmailDomain>(
      `/api/v1/businesses/${businessId}/email-domains/${domainId}/verify`,
      {},
    );
  }

  // GET /businesses/{id}/inbound-addresses — keyset-paginated list of the system
  // address plus any custom addresses.
  listInboundAddresses(businessId: string, cursor?: string): Observable<Page<InboundAddress>> {
    let params = new HttpParams();
    if (cursor) params = params.set('cursor', cursor);
    return this.http.get<Page<InboundAddress>>(
      `/api/v1/businesses/${businessId}/inbound-addresses`,
      { params },
    );
  }

  // POST /businesses/{id}/inbound-addresses — add a custom address on a verified,
  // owned domain. Returns the created InboundAddress (201). 400 invalid/off-domain
  // address; 404 unknown/unauthorized domain; 409 domain not verified yet OR duplicate.
  createInboundAddress(
    businessId: string,
    body: { address: string; email_domain_id: string },
  ): Observable<InboundAddress> {
    return this.http.post<InboundAddress>(
      `/api/v1/businesses/${businessId}/inbound-addresses`,
      body,
    );
  }
}
