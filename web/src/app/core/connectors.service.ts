import { HttpClient } from '@angular/common/http';
import { Injectable, inject, signal } from '@angular/core';
import { Observable, tap } from 'rxjs';

export interface ConnectorHealth {
  state: 'healthy' | 'degraded' | 'disabled';
  linked_ticket_count: number;
  pending_outbound_ops: number;
  failed_outbound_ops: number;
  last_error: string | null;
}

export interface Connector {
  id: string;
  business_id: string;
  type: string;
  display_name: string;
  base_url: string;
  allow_private_base_url: boolean;
  config: Record<string, unknown>;
  status: string;
  last_reconciled_at: string | null;
  created_at: string;
  updated_at: string;
  health: ConnectorHealth;
}

export interface CreateConnectorBody {
  type: string;
  display_name: string;
  base_url: string;
  allow_private_base_url?: boolean;
  email: string;
  api_token: string;
  webhook_secret?: string;
  config?: Record<string, unknown>;
}

export interface UpdateConnectorBody {
  display_name?: string;
  config?: Record<string, unknown>;
  status?: 'enabled' | 'disabled';
}

export interface RotateCredentialBody {
  email: string;
  api_token: string;
  webhook_secret?: string;
}

export interface TestResult {
  ok: boolean;
  detail: string;
}

// ConnectorsService talks to the connectors.manage API. degradedCount drives the nav badge:
// it counts connectors that are NOT healthy (degraded or disabled) for the current business.
@Injectable({ providedIn: 'root' })
export class ConnectorsService {
  private http = inject(HttpClient);
  readonly degradedCount = signal(0);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}/connectors`;
  }

  list(businessId: string): Observable<{ items: Connector[] }> {
    return this.http
      .get<{ items: Connector[] }>(this.base(businessId))
      .pipe(tap((r) => this.degradedCount.set((r.items ?? []).filter((c) => c.health.state !== 'healthy').length)));
  }
  create(businessId: string, body: CreateConnectorBody): Observable<Connector> {
    return this.http.post<Connector>(this.base(businessId), body);
  }
  update(businessId: string, id: string, body: UpdateConnectorBody): Observable<Connector> {
    return this.http.patch<Connector>(`${this.base(businessId)}/${id}`, body);
  }
  rotate(businessId: string, id: string, body: RotateCredentialBody): Observable<Connector> {
    return this.http.put<Connector>(`${this.base(businessId)}/${id}/credential`, body);
  }
  test(businessId: string, id: string): Observable<TestResult> {
    return this.http.post<TestResult>(`${this.base(businessId)}/${id}/test`, {});
  }
  remove(businessId: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/${id}`);
  }
  refreshCount(businessId: string): void {
    this.list(businessId).subscribe({ error: () => this.degradedCount.set(0) });
  }
}
