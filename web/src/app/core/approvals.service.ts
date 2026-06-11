import { HttpClient } from '@angular/common/http';
import { Injectable, inject, signal } from '@angular/core';
import { Observable, tap } from 'rxjs';

export interface ApprovalItem {
  id: string;
  agent_run_id: string;
  tool: string;
  effect_class: number;
  state: string;
  expires_at: string;
  summary: string;
}

@Injectable({ providedIn: 'root' })
export class ApprovalsService {
  private http = inject(HttpClient);
  readonly pendingCount = signal(0);

  listPending(businessId: string): Observable<{ items: ApprovalItem[] }> {
    return this.http
      .get<{ items: ApprovalItem[] }>(`/api/v1/businesses/${businessId}/approvals`)
      .pipe(tap((r) => this.pendingCount.set(r.items.length)));
  }
  approve(businessId: string, id: string): Observable<ApprovalItem> {
    return this.http.post<ApprovalItem>(`/api/v1/businesses/${businessId}/approvals/${id}/approve`, {});
  }
  deny(businessId: string, id: string): Observable<ApprovalItem> {
    return this.http.post<ApprovalItem>(`/api/v1/businesses/${businessId}/approvals/${id}/deny`, {});
  }
  refreshCount(businessId: string): void {
    this.listPending(businessId).subscribe({ error: () => this.pendingCount.set(0) });
  }
}
