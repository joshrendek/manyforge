import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

export interface PublicMailingSubscription {
  email: string;
  first_name?: string | null;
  last_name?: string | null;
  attributes?: Record<string, unknown>;
  website?: string;
}

@Injectable({ providedIn: 'root' })
export class PublicMailingService {
  private http = inject(HttpClient);

  subscribe(key: string, body: PublicMailingSubscription): Observable<{ accepted: boolean }> {
    return this.http.post<{ accepted: boolean }>(
      `/api/v1/mailing/public/${encodeURIComponent(key)}/subscribe`,
      body,
    );
  }

  confirm(token: string): Observable<string> {
    return this.http.post(`/m/confirm/${encodeURIComponent(token)}`, null, {
      responseType: 'text',
    });
  }

  unsubscribe(token: string): Observable<string> {
    return this.http.post(`/m/u/${encodeURIComponent(token)}`, null, { responseType: 'text' });
  }
}
