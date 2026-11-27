import { HttpClient, HttpParams } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';
import { Page } from './ticket.service';

// PortalDomain is the API view of a business's custom portal hostname
// (api/openapi.yaml `portal-domains`). Snake_case matches the wire format.
export interface PortalDnsRecord {
  name: string;
  value: string;
}

export interface PortalDomain {
  id: string;
  hostname: string;
  verified: boolean;
  verified_at: string | null;
  dns: {
    verification_txt: PortalDnsRecord;
    // value is '' when the instance has no public address configured.
    target: { type: string; name: string; value: string };
  };
  created_at: string;
}

@Injectable({ providedIn: 'root' })
export class PortalDomainService {
  private http = inject(HttpClient);

  private url(businessId: string): string {
    return `/api/v1/businesses/${businessId}/portal-domains`;
  }

  // GET — keyset-paginated list; first page limit 50 unless a cursor is given.
  list(businessId: string, cursor?: string): Observable<Page<PortalDomain>> {
    let params = new HttpParams().set('limit', '50');
    if (cursor) params = params.set('cursor', cursor);
    return this.http.get<Page<PortalDomain>>(this.url(businessId), { params });
  }

  // POST — 201 PortalDomain; 400 validation; 409 conflict.
  create(businessId: string, hostname: string): Observable<PortalDomain> {
    return this.http.post<PortalDomain>(this.url(businessId), { hostname });
  }

  // POST — 200 PortalDomain. Still-unverified is a pending poll, NOT an error.
  verify(businessId: string, domainId: string): Observable<PortalDomain> {
    return this.http.post<PortalDomain>(`${this.url(businessId)}/${domainId}/verify`, {});
  }

  // DELETE — 204.
  remove(businessId: string, domainId: string): Observable<void> {
    return this.http.delete<void>(`${this.url(businessId)}/${domainId}`);
  }
}
