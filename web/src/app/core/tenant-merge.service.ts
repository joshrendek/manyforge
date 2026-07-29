import { HttpClient, HttpHeaders } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

export type TenantMergeStatus = 'preflight_required' | 'ready' | 'running' | 'succeeded' | 'failed';

export interface TenantMergeDestination {
  id: string;
  name: string;
  tenant_root_id: string;
  tenant_root_name: string;
  hierarchy_path: string;
  is_tenant_root: boolean;
}

export interface TenantMergeSourceOptions {
  source_root_id: string;
  source_root_name: string;
  destinations: TenantMergeDestination[];
}

export interface TenantMergeFinding {
  code: string;
  module: string;
  object: string;
  count: number;
  limit?: number;
  bytes?: number;
  details?: Record<string, unknown>;
}

export interface TenantMergeCount {
  rows: number;
  bytes: number;
}

export interface TenantMergeFailure {
  code: string;
  stage: string;
  operator_correlation_id: string;
}

export interface TenantMergeOperation {
  id: string;
  correlation_id: string;
  source_root_id: string;
  destination_parent_id: string;
  destination_root_id: string;
  status: TenantMergeStatus;
  preflight_generation?: string | null;
  module_counts: Record<string, TenantMergeCount>;
  conflicts: TenantMergeFinding[];
  warnings: TenantMergeFinding[];
  affected_rows: number;
  estimated_bytes: number;
  source_businesses?: number | null;
  resulting_depth?: number | null;
  attachment_count: number;
  attachment_bytes: number;
  preflight_completed_at?: string | null;
  ready_at?: string | null;
  confirmed_at?: string | null;
  created_at: string;
  updated_at: string;
  failure?: TenantMergeFailure | null;
}

@Injectable({ providedIn: 'root' })
export class TenantMergeService {
  private http = inject(HttpClient);

  options(): Observable<{ sources: TenantMergeSourceOptions[] }> {
    return this.http.get<{ sources: TenantMergeSourceOptions[] }>('/api/v1/tenant-merge-options');
  }

  create(
    sourceRootId: string,
    destinationParentId: string,
    idempotencyKey: string,
  ): Observable<TenantMergeOperation> {
    return this.http.post<TenantMergeOperation>(
      `/api/v1/businesses/${sourceRootId}/tenant-merges`,
      { destination_parent_id: destinationParentId },
      { headers: new HttpHeaders({ 'Idempotency-Key': idempotencyKey }) },
    );
  }

  get(operationId: string): Observable<TenantMergeOperation> {
    return this.http.get<TenantMergeOperation>(`/api/v1/tenant-merges/${operationId}`);
  }

  preflight(operationId: string): Observable<TenantMergeOperation> {
    return this.http.post<TenantMergeOperation>(
      `/api/v1/tenant-merges/${operationId}/preflight`,
      {},
    );
  }

  confirm(
    operationId: string,
    sourceName: string,
    destinationName: string,
    password: string,
  ): Observable<TenantMergeOperation> {
    return this.http.post<TenantMergeOperation>(`/api/v1/tenant-merges/${operationId}/confirm`, {
      source_name: sourceName,
      destination_name: destinationName,
      password,
    });
  }
}
