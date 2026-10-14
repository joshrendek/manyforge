import { HttpClient, HttpErrorResponse } from '@angular/common/http';
import { Injectable, inject, signal } from '@angular/core';
import { Observable, tap, throwError } from 'rxjs';

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

// Route params (businessId, project id, task key, token id) reach these URLs
// straight from the address bar. encodeURIComponent leaves `..` intact, so a
// crafted link could walk the authenticated request onto another API path
// (client-side path traversal). Every segment must match its known shape.
// Ids and slugs: no `.`, `/`, `%`, `?` or `#`, so nothing can climb or re-route.
const ID_RE = /^[A-Za-z0-9_-]{1,64}$/;
const KEY_RE = /^[a-z][a-z0-9]{0,11}-[a-z0-9]{2,16}(?:\.[0-9]{1,4}){0,3}$/;

export class InvalidPathSegmentError extends Error {}

export function segment(value: string, shape: RegExp): string {
  if (!shape.test(value)) {
    throw new InvalidPathSegmentError(`invalid path segment: ${JSON.stringify(value)}`);
  }
  return encodeURIComponent(value);
}

// guard turns a rejected segment into the same 404 the API returns for an
// unknown resource, so callers' existing error handling applies.
function guard<T>(build: () => Observable<T>): Observable<T> {
  try {
    return build();
  } catch (e) {
    if (e instanceof InvalidPathSegmentError) {
      return throwError(() => new HttpErrorResponse({ status: 404, statusText: 'Not Found' }));
    }
    throw e;
  }
}

@Injectable({ providedIn: 'root' })
export class WorkService {
  private http = inject(HttpClient);
  // Drives the Work nav badge: open operator-lane tasks for the current business.
  readonly openOperatorCount = signal(0);

  private base(businessId: string): string {
    return `/api/v1/businesses/${segment(businessId, ID_RE)}/work`;
  }

  private project(businessId: string, projectId: string): string {
    return `${this.base(businessId)}/projects/${segment(projectId, ID_RE)}`;
  }

  summary(businessId: string): Observable<WorkSummary> {
    return guard(() =>
      this.http
        .get<WorkSummary>(`${this.base(businessId)}/summary`)
        .pipe(tap((r) => this.openOperatorCount.set(r.open_operator ?? 0))),
    );
  }

  // Re-reads the badge after a mutation; failures zero it like the shell's poll does.
  refreshSummary(businessId: string): void {
    this.summary(businessId).subscribe({ error: () => this.openOperatorCount.set(0) });
  }

  listProjects(businessId: string): Observable<{ items: WorkProject[] }> {
    return guard(() =>
      this.http.get<{ items: WorkProject[] }>(`${this.base(businessId)}/projects`),
    );
  }

  createProject(
    businessId: string,
    body: { name: string; slug?: string; key_prefix?: string },
  ): Observable<WorkProject> {
    return guard(() => this.http.post<WorkProject>(`${this.base(businessId)}/projects`, body));
  }

  getProject(businessId: string, projectId: string): Observable<WorkProject> {
    return guard(() => this.http.get<WorkProject>(this.project(businessId, projectId)));
  }

  listTasks(
    businessId: string,
    projectId: string,
    opts: { includeClosed?: boolean } = {},
  ): Observable<{ items: WorkTask[] }> {
    const q = opts.includeClosed ? '?include_closed=true' : '';
    return guard(() =>
      this.http.get<{ items: WorkTask[] }>(`${this.project(businessId, projectId)}/tasks${q}`),
    );
  }

  createTask(
    businessId: string,
    projectId: string,
    body: WorkTaskFields & { title: string },
  ): Observable<WorkTask> {
    return guard(() =>
      this.http.post<WorkTask>(`${this.project(businessId, projectId)}/tasks`, body),
    );
  }

  updateTask(
    businessId: string,
    projectId: string,
    key: string,
    patch: WorkTaskPatch,
  ): Observable<WorkTask> {
    return guard(() =>
      this.http.patch<WorkTask>(
        `${this.project(businessId, projectId)}/tasks/${segment(key, KEY_RE)}`,
        patch,
      ),
    );
  }

  listTokens(businessId: string, projectId: string): Observable<{ items: WorkToken[] }> {
    return guard(() =>
      this.http.get<{ items: WorkToken[] }>(`${this.project(businessId, projectId)}/tokens`),
    );
  }

  createToken(businessId: string, projectId: string, name: string): Observable<CreatedWorkToken> {
    return guard(() =>
      this.http.post<CreatedWorkToken>(`${this.project(businessId, projectId)}/tokens`, {
        name,
      }),
    );
  }

  revokeToken(businessId: string, projectId: string, tokenId: string): Observable<void> {
    return guard(() =>
      this.http.delete<void>(
        `${this.project(businessId, projectId)}/tokens/${segment(tokenId, ID_RE)}`,
      ),
    );
  }
}
