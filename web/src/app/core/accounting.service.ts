import { HttpClient, HttpParams } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';
import { Page } from './ticket.service';

// Typed client for the agent accounting (US7) API. Thin by design — mirrors
// ticket.service.ts: hard-coded /api/v1 URLs, typed Observables, no auth
// handling (the global authInterceptor attaches the bearer + retries on 401).

export type WindowName = 'this_month' | 'last_month' | 'last_30_days' | 'custom';

export interface AgentUsage {
  agent_id: string;
  name: string;
  monthly_budget_cents: number;
  run_count: number;
  tokens_in: number;
  tokens_out: number;
  cost_cents: number;
  budget_pct?: number | null;
}

export interface AccountingSummary {
  window: { from: string; to: string };
  totals: { cost_cents: number; tokens_in: number; tokens_out: number; run_count: number };
  agents: AgentUsage[];
}

export interface RunSummary {
  id: string;
  agent_id: string;
  trigger: string;
  status: string;
  tokens_in: number;
  tokens_out: number;
  cost_cents: number;
  correlation_id: string;
  error?: string | null;
  created_at: string;
}

// Keyset page of RunSummary rows — reuses the shared Page<T> envelope from
// ticket.service.ts rather than duplicating the { items, next_cursor } shape.
export type RunPage = Page<RunSummary>;

export interface RunListFilters {
  status?: string;
  window?: WindowName;
  from?: string;
  to?: string;
  cursor?: string;
  limit?: number;
}

@Injectable({ providedIn: 'root' })
export class AccountingService {
  private http = inject(HttpClient);

  // GET /businesses/{id}/accounting — cost/usage summary for the given window.
  // window defaults to this_month when omitted; use 'custom' + from/to for an
  // arbitrary range (requires agents.read; 404 for unknown/unauthorized business).
  getSummary(
    businessId: string,
    window?: WindowName,
    from?: string,
    to?: string,
  ): Observable<AccountingSummary> {
    let params = new HttpParams();
    if (window) params = params.set('window', window);
    if (from) params = params.set('from', from);
    if (to) params = params.set('to', to);
    return this.http.get<AccountingSummary>(`/api/v1/businesses/${businessId}/accounting`, {
      params,
    });
  }

  // GET /businesses/{id}/agents/{agentId}/runs — keyset-paginated run history
  // for a single agent, optional status/window/date-range filters
  // (requires agents.read; 404 for unknown/unauthorized agent or business).
  listRuns(
    businessId: string,
    agentId: string,
    filters: RunListFilters = {},
  ): Observable<RunPage> {
    let params = new HttpParams();
    if (filters.status) params = params.set('status', filters.status);
    if (filters.window) params = params.set('window', filters.window);
    if (filters.from) params = params.set('from', filters.from);
    if (filters.to) params = params.set('to', filters.to);
    if (filters.cursor) params = params.set('cursor', filters.cursor);
    if (filters.limit != null) params = params.set('limit', String(filters.limit));
    return this.http.get<RunPage>(
      `/api/v1/businesses/${businessId}/agents/${agentId}/runs`,
      { params },
    );
  }
}
