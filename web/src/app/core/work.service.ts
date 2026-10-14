import { HttpClient } from '@angular/common/http';
import { Injectable, inject, signal } from '@angular/core';
import { Observable, tap } from 'rxjs';

// Work Board (spec 018): per-business work projects whose tasks live in an `agent` lane (picked
// up by coding agents through the `mf` CLI) or an `operator` lane (things only a human can do).
// Shapes mirror the spec's snake_case wire format exactly; every Task field is always present.

export type WorkLane = 'agent' | 'operator';
export type WorkStatus = 'backlog' | 'todo' | 'in_progress' | 'blocked' | 'done' | 'canceled';
export type WorkType = 'task' | 'bug' | 'feature' | 'chore';

export interface WorkProjectCounts {
  backlog: number;
  todo: number;
  in_progress: number;
  blocked: number;
  operator_open: number;
}

export interface WorkProject {
  id: string;
  slug: string;
  name: string;
  key_prefix: string;
  created_at: string;
  // Present on the list response; the single-project GET may omit it.
  counts?: WorkProjectCounts;
}

export interface WorkTask {
  key: string;
  title: string;
  description: string;
  notes: string;
  lane: WorkLane;
  status: WorkStatus;
  // 0–4, 0 = critical (beads semantics).
  priority: number;
  type: WorkType;
  assignee: string;
  labels: string[];
  blocked_by: string[];
  rev: number;
  created_at: string;
  updated_at: string;
  closed_at: string | null;
}

export interface WorkTaskFields {
  title?: string;
  description?: string;
  lane?: WorkLane;
  status?: WorkStatus;
  priority?: number;
  type?: WorkType;
  assignee?: string;
  labels?: string[];
  blocked_by?: string[];
}

// PATCH accepts any task field plus an optional `note`, appended server-side with the caller as
// author.
export interface WorkTaskPatch extends WorkTaskFields {
  note?: string;
}

export interface WorkToken {
  id: string;
  name: string;
  prefix: string;
  created_at: string;
  last_used_at: string | null;
  revoked_at: string | null;
}

// The create response is the only place the plaintext `mfw_…` token ever appears.
export interface CreatedWorkToken {
  id: string;
  name: string;
  prefix: string;
  created_at: string;
  token: string;
}

export interface WorkSummary {
  open_operator: number;
  open_agent: number;
  in_progress: number;
}

@Injectable({ providedIn: 'root' })
export class WorkService {
  private http = inject(HttpClient);
  // Drives the Work nav badge: open operator-lane tasks for the current business.
  readonly openOperatorCount = signal(0);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}/work`;
  }

  private project(businessId: string, projectId: string): string {
    return `${this.base(businessId)}/projects/${encodeURIComponent(projectId)}`;
  }

  summary(businessId: string): Observable<WorkSummary> {
    return this.http
      .get<WorkSummary>(`${this.base(businessId)}/summary`)
      .pipe(tap((r) => this.openOperatorCount.set(r.open_operator ?? 0)));
  }

  // Re-reads the badge after a mutation; failures zero it like the shell's poll does.
  refreshSummary(businessId: string): void {
    this.summary(businessId).subscribe({ error: () => this.openOperatorCount.set(0) });
  }

  listProjects(businessId: string): Observable<{ items: WorkProject[] }> {
    return this.http.get<{ items: WorkProject[] }>(`${this.base(businessId)}/projects`);
  }

  createProject(
    businessId: string,
    body: { name: string; slug?: string; key_prefix?: string },
  ): Observable<WorkProject> {
    return this.http.post<WorkProject>(`${this.base(businessId)}/projects`, body);
  }

  getProject(businessId: string, projectId: string): Observable<WorkProject> {
    return this.http.get<WorkProject>(this.project(businessId, projectId));
  }

  listTasks(
    businessId: string,
    projectId: string,
    opts: { includeClosed?: boolean } = {},
  ): Observable<{ items: WorkTask[] }> {
    const q = opts.includeClosed ? '?include_closed=true' : '';
    return this.http.get<{ items: WorkTask[] }>(`${this.project(businessId, projectId)}/tasks${q}`);
  }

  createTask(
    businessId: string,
    projectId: string,
    body: WorkTaskFields & { title: string },
  ): Observable<WorkTask> {
    return this.http.post<WorkTask>(`${this.project(businessId, projectId)}/tasks`, body);
  }

  updateTask(
    businessId: string,
    projectId: string,
    key: string,
    patch: WorkTaskPatch,
  ): Observable<WorkTask> {
    return this.http.patch<WorkTask>(
      `${this.project(businessId, projectId)}/tasks/${encodeURIComponent(key)}`,
      patch,
    );
  }

  listTokens(businessId: string, projectId: string): Observable<{ items: WorkToken[] }> {
    return this.http.get<{ items: WorkToken[] }>(`${this.project(businessId, projectId)}/tokens`);
  }

  createToken(businessId: string, projectId: string, name: string): Observable<CreatedWorkToken> {
    return this.http.post<CreatedWorkToken>(`${this.project(businessId, projectId)}/tokens`, {
      name,
    });
  }

  revokeToken(businessId: string, projectId: string, tokenId: string): Observable<void> {
    return this.http.delete<void>(
      `${this.project(businessId, projectId)}/tokens/${encodeURIComponent(tokenId)}`,
    );
  }
}
