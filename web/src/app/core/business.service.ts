import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';
import { Business } from './tree';

// Typed client for the tenancy (businesses) API. Thin by design — the hierarchy
// logic lives in tree.ts and the service layer in Go; this just maps calls.
@Injectable({ providedIn: 'root' })
export class BusinessService {
  private http = inject(HttpClient);

  list(): Observable<{ items: Business[] }> {
    return this.http.get<{ items: Business[] }>('/api/v1/businesses');
  }

  create(name: string, parentId?: string): Observable<Business> {
    const body = parentId ? { name, parent_id: parentId } : { name };
    return this.http.post<Business>('/api/v1/businesses', body);
  }

  rename(id: string, name: string): Observable<unknown> {
    return this.http.patch(`/api/v1/businesses/${id}`, { name });
  }

  move(id: string, newParentId: string): Observable<unknown> {
    return this.http.post(`/api/v1/businesses/${id}/move`, { new_parent_id: newParentId });
  }

  archive(id: string): Observable<unknown> {
    return this.http.post(`/api/v1/businesses/${id}/archive`, {});
  }

  restore(id: string): Observable<unknown> {
    return this.http.post(`/api/v1/businesses/${id}/restore`, {});
  }

  remove(id: string): Observable<unknown> {
    return this.http.delete(`/api/v1/businesses/${id}`, { body: { confirm: true } });
  }
}
