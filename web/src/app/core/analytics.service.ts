import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

// Web analytics (spec 011 / manyforge-as0) plus the telemetry-client registration it reports on
// (spec 010 / manyforge-p20). Shapes mirror the Go response DTOs (snake_case) exactly.

// A registered telemetry client. For analytics this is "a site".
//
// publishable_key is PUBLIC — it is designed to be pasted into a web page, so it is safe to
// display with a copy button, unlike a masked token.
export interface TelemetryClient {
  id: string;
  business_id: string;
  tenant_root_id: string;
  kind: 'analytics' | 'crash';
  name: string;
  publishable_key: string;
  status: string;
  // Whether ingest DEMANDS an HMAC for this client. False is the embeddable mode.
  require_signature: boolean;
  // Whether a signing secret exists. The secret itself is never returned after creation.
  has_secret: boolean;
  // Write-once signing secret (`mfs_…`). Present ONLY in the create response; the API never
  // returns it again. The UI must show it exactly once and must not try to re-fetch it.
  secret?: string;
  created_at: string;
  revoked_at?: string | null;
}

export interface TelemetryMoveTarget {
  id: string;
  tenant_root_id: string;
  name: string;
}

export interface DayPoint {
  date: string;
  pageviews: number;
  visitors: number;
}

export interface PathCount {
  path: string;
  pageviews: number;
  visitors: number;
}

export interface HostCount {
  host: string;
  pageviews: number;
  visitors: number;
}

export interface ValueCount {
  value: string;
  pageviews: number;
  visitors: number;
}

// One card on the multi-site overview grid.
//
// `visitors` here is the SAME measure the per-site dashboard headlines — peak daily uniques, not a
// sum — so a card and the dashboard it opens agree. See the API docs for why cross-day dedupe is
// impossible under the rotating-salt privacy model.
export interface OverviewSite {
  client_id: string;
  name: string;
  business_id: string;
  business_name: string;
  pageviews: number;
  visitors: number;
  series: DayPoint[];
}

export interface AnalyticsSummary {
  from: string;
  to: string;
  pageviews: number;
  // PEAK DAILY unique visitors across the window — not a sum and not a cross-day total. The
  // visitor hash rotates daily by design, so no cross-day identifier exists to deduplicate with.
  visitors: number;
  direct_pageviews: number;
  series: DayPoint[];
  top_pages: PathCount[];
  top_referrers: HostCount[];
  // Keyed by dimension: event, utm_source, utm_medium, utm_campaign, device, browser, country.
  // A tracked dimension with no data is present but empty, so the UI can tell "nothing collected"
  // apart from "not a dimension we track".
  breakdowns: Record<string, ValueCount[]>;
}

@Injectable({ providedIn: 'root' })
export class AnalyticsService {
  private http = inject(HttpClient);

  listClients(businessId: string): Observable<{ clients: TelemetryClient[] }> {
    return this.http.get<{ clients: TelemetryClient[] }>(
      `/api/v1/businesses/${businessId}/telemetry/clients`,
    );
  }

  createClient(
    businessId: string,
    body: { kind: 'analytics' | 'crash'; name: string; require_signature: boolean },
  ): Observable<TelemetryClient> {
    return this.http.post<TelemetryClient>(
      `/api/v1/businesses/${businessId}/telemetry/clients`,
      body,
    );
  }

  revokeClient(businessId: string, clientId: string): Observable<TelemetryClient> {
    return this.http.post<TelemetryClient>(
      `/api/v1/businesses/${businessId}/telemetry/clients/${clientId}/revoke`,
      {},
    );
  }

  moveTargets(
    businessId: string,
    clientId: string,
  ): Observable<{ targets: TelemetryMoveTarget[] }> {
    return this.http.get<{ targets: TelemetryMoveTarget[] }>(
      `/api/v1/businesses/${businessId}/telemetry/clients/${clientId}/move-targets`,
    );
  }

  moveClient(
    sourceBusinessId: string,
    clientId: string,
    targetBusinessId: string,
  ): Observable<TelemetryClient> {
    return this.http.post<TelemetryClient>(
      `/api/v1/businesses/${sourceBusinessId}/telemetry/clients/${clientId}/move`,
      { target_business_id: targetBusinessId },
    );
  }

  // Every site the caller can read, across every business. No business id: the server decides
  // which businesses qualify, from the caller's permissions.
  overview(days: number): Observable<{ sites: OverviewSite[] }> {
    return this.http.get<{ sites: OverviewSite[] }>(`/api/v1/analytics/overview?days=${days}`);
  }

  summary(businessId: string, clientId: string, days: number): Observable<AnalyticsSummary> {
    return this.http.get<AnalyticsSummary>(
      `/api/v1/businesses/${businessId}/analytics/summary?client_id=${clientId}&days=${days}`,
    );
  }

  // The embed tag a tenant pastes into their site. Built from the browser's own origin so it is
  // correct in dev, on a preview host, and in production without configuration.
  embedSnippet(publishableKey: string): string {
    const origin = typeof location !== 'undefined' ? location.origin : '';
    return `<script defer src="${origin}/a.js" data-key="${publishableKey}"></script>`;
  }
}
