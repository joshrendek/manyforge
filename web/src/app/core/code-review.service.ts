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
