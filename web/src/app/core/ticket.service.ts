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
  created_at: string;
}

// Keyset page envelope (openapi.yaml:319-321): { items, next_cursor }.
export interface Page<T> {
  items: T[];
  next_cursor: string | null;
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
}
