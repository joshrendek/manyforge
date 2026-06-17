import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

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

@Injectable({ providedIn: 'root' })
export class CrmService {
  private http = inject(HttpClient);

  private base(b: string): string {
    return `/api/v1/businesses/${b}`;
  }

  listContacts(b: string, cursor?: string): Observable<{ items: Contact[]; next_cursor?: string | null }> {
    const q = cursor ? `?cursor=${encodeURIComponent(cursor)}` : '';
    return this.http.get<{ items: Contact[]; next_cursor?: string | null }>(`${this.base(b)}/contacts${q}`);
  }

  getContact(b: string, id: string): Observable<Contact> {
    return this.http.get<Contact>(`${this.base(b)}/contacts/${id}`);
  }

  createContact(b: string, body: { primary_email: string; display_name?: string }): Observable<Contact> {
    return this.http.post<Contact>(`${this.base(b)}/contacts`, body);
  }

  updateContact(b: string, id: string, body: Partial<{ display_name: string; company_id: string }>): Observable<Contact> {
    return this.http.patch<Contact>(`${this.base(b)}/contacts/${id}`, body);
  }

  deleteContact(b: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(b)}/contacts/${id}`);
  }

  mergeContact(b: string, winnerId: string, loserId: string): Observable<{ status: string }> {
    return this.http.post<{ status: string }>(`${this.base(b)}/contacts/${winnerId}/merge`, { loser_id: loserId });
  }

  listCompanies(b: string, cursor?: string): Observable<{ items: Company[]; next_cursor?: string | null }> {
    const q = cursor ? `?cursor=${encodeURIComponent(cursor)}` : '';
    return this.http.get<{ items: Company[]; next_cursor?: string | null }>(`${this.base(b)}/companies${q}`);
  }

  getCompany(b: string, id: string): Observable<Company> {
    return this.http.get<Company>(`${this.base(b)}/companies/${id}`);
  }

  createCompany(b: string, body: { name: string; domain?: string }): Observable<Company> {
    return this.http.post<Company>(`${this.base(b)}/companies`, body);
  }

  updateCompany(b: string, id: string, body: Partial<{ name: string; domain: string }>): Observable<Company> {
    return this.http.patch<Company>(`${this.base(b)}/companies/${id}`, body);
  }

  deleteCompany(b: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(b)}/companies/${id}`);
  }
}
