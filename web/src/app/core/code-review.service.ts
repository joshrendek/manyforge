import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

export type RepoConnectorStatus = 'active' | 'inactive' | 'error';
export type CodeReviewStatus = 'pending' | 'running' | 'succeeded' | 'failed';
export type FindingSeverity = 'info' | 'warning' | 'error';

export interface RepoConnector {
  id: string;
  type: string;
  display_name: string;
  base_url: string;
  repo: string;
  allow_private_base_url: boolean;
  status: RepoConnectorStatus;
  created_at: string;
}

export interface CreateRepoConnectorBody {
  type: 'github';
  display_name: string;
  base_url: string;
  repo: string;
  api_token: string;
  allow_private_base_url: boolean;
}

export interface Finding {
  file: string;
  line: number | null;
  severity: FindingSeverity;
  title: string;
  detail: string;
  // Which review lane produced this finding (spec 008). Empty/absent on a legacy
  // single-lane review; may be a joined set (e.g. "correctness, security") when the
  // same issue was flagged by multiple lanes and de-duplicated during aggregation.
  dimension?: string;
}

export type DimensionRunStatus = 'succeeded' | 'failed' | 'skipped';

// DimensionRun mirrors the Go coding.dimensionRun record persisted in
// code_review.dimension_runs (spec 008): per-lane accounting for a multi-dimension
// review. A "skipped" lane carries skipped_reason and did not run this review.
export interface DimensionRun {
  dimension: string;
  model?: string;
  provider?: string;
  tokens_in: number;
  tokens_out: number;
  cost_cents: number;
  status: DimensionRunStatus;
  skipped_reason?: string;
  finding_count: number;
}

export interface CodeReview {
  id: string;
  status: CodeReviewStatus;
  summary: string;
  review_url: string;
  pr_number: number;
  model: string;
  findings: Finding[];
  findings_count: number;
  cost_cents: number;
  created_at: string;
  posted_at: string | null;
  progress?: { phase: string; tokens: number; preview: string };
  // Per-lane accounting for a multi-dimension review (spec 008); absent on legacy
  // single-lane reviews. The detail page groups findings by dimension and surfaces
  // skipped lanes from this.
  dimension_runs?: DimensionRun[];
}

export interface TriggerBody {
  agent_id: string;
  repo_connector_id: string;
  pr_number: number;
}

export interface TriggerReviewResponse {
  id: string;
  status: CodeReviewStatus;
  review_url: string;
}

@Injectable({ providedIn: 'root' })
export class CodeReviewService {
  private http = inject(HttpClient);

  private connectorsBase(businessId: string): string {
    return `/api/v1/businesses/${businessId}/repo-connectors`;
  }

  private reviewsBase(businessId: string): string {
    return `/api/v1/businesses/${businessId}/code-reviews`;
  }

  listConnectors(businessId: string): Observable<{ items: RepoConnector[] }> {
    return this.http.get<{ items: RepoConnector[] }>(this.connectorsBase(businessId));
  }

  createConnector(businessId: string, body: CreateRepoConnectorBody): Observable<{ id: string }> {
    return this.http.post<{ id: string }>(this.connectorsBase(businessId), body);
  }

  deleteConnector(businessId: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.connectorsBase(businessId)}/${id}`);
  }

  listReviews(businessId: string): Observable<{ items: CodeReview[] }> {
    return this.http.get<{ items: CodeReview[] }>(this.reviewsBase(businessId));
  }

  getReview(businessId: string, id: string): Observable<CodeReview> {
    return this.http.get<CodeReview>(`${this.reviewsBase(businessId)}/${id}`);
  }

  trigger(businessId: string, body: TriggerBody): Observable<TriggerReviewResponse> {
    return this.http.post<TriggerReviewResponse>(this.reviewsBase(businessId), body);
  }
}
