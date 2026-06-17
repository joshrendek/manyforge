import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';
import { Page } from './ticket.service';

// Contacts and companies are tenant-wide records, but the API is reached through a
// business-scoped URL (the business resolves the tenant root). Shapes mirror the
// schemas in specs/005-crm-contacts-timeline/contracts/openapi.yaml exactly.
export interface Contact {
  id: string;
  tenant_root_id: string;
  primary_email: string;
  display_name?: string | null;
  company_id?: string | null;
  created_at: string;
  updated_at: string;
}

export interface Company {
  id: string;
  tenant_root_id: string;
  name: string;
  domain?: string | null;
  created_at: string;
  updated_at: string;
}

// A single entry in a contact's activity timeline. Snake_case fields mirror the
// backend ActivityEntry DTO exactly.
export interface ActivityEntry {
  id: string;
  tenant_root_id: string;
  business_id: string;
  contact_id: string;
  kind: string;
  occurred_at: string;
  actor: string | null;
  source_type: string;
  source_id: string | null;
  summary: string;
  metadata?: unknown;
  created_at: string;
}

@Injectable({ providedIn: 'root' })
export class CrmService {
  private http = inject(HttpClient);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}`;
  }

  listContacts(businessId: string, cursor?: string): Observable<Page<Contact>> {
    const q = cursor ? `?cursor=${encodeURIComponent(cursor)}` : '';
    return this.http.get<Page<Contact>>(`${this.base(businessId)}/contacts${q}`);
  }

  getContact(businessId: string, id: string): Observable<Contact> {
    return this.http.get<Contact>(`${this.base(businessId)}/contacts/${id}`);
  }

  createContact(businessId: string, body: { primary_email: string; display_name?: string }): Observable<Contact> {
    return this.http.post<Contact>(`${this.base(businessId)}/contacts`, body);
  }

  updateContact(businessId: string, id: string, body: Partial<{ display_name: string; company_id: string }>): Observable<Contact> {
    return this.http.patch<Contact>(`${this.base(businessId)}/contacts/${id}`, body);
  }

  deleteContact(businessId: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/contacts/${id}`);
  }

  mergeContact(businessId: string, winnerId: string, loserId: string): Observable<{ status: string }> {
    return this.http.post<{ status: string }>(`${this.base(businessId)}/contacts/${winnerId}/merge`, { loser_id: loserId });
  }

  listCompanies(businessId: string, cursor?: string): Observable<Page<Company>> {
    const q = cursor ? `?cursor=${encodeURIComponent(cursor)}` : '';
    return this.http.get<Page<Company>>(`${this.base(businessId)}/companies${q}`);
  }

  getCompany(businessId: string, id: string): Observable<Company> {
    return this.http.get<Company>(`${this.base(businessId)}/companies/${id}`);
  }

  createCompany(businessId: string, body: { name: string; domain?: string }): Observable<Company> {
    return this.http.post<Company>(`${this.base(businessId)}/companies`, body);
  }

  updateCompany(businessId: string, id: string, body: Partial<{ name: string; domain: string }>): Observable<Company> {
    return this.http.patch<Company>(`${this.base(businessId)}/companies/${id}`, body);
  }

  deleteCompany(businessId: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/companies/${id}`);
  }

  listActivity(businessId: string, contactId: string, cursor?: string): Observable<Page<ActivityEntry>> {
    const q = cursor ? `?cursor=${encodeURIComponent(cursor)}` : '';
    return this.http.get<Page<ActivityEntry>>(`${this.base(businessId)}/contacts/${contactId}/activity${q}`);
  }
}
