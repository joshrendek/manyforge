import { HttpClient, HttpParams } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';
import { Page } from './ticket.service';

export type AutomationStatus = 'draft' | 'active' | 'paused' | 'archived';
export type AutomationVersionStatus = 'draft' | 'active' | 'superseded';
export type AutomationNodeKind =
  | 'trigger'
  | 'send_email'
  | 'wait'
  | 'condition'
  | 'add_tag'
  | 'remove_tag'
  | 'exit';
export type AutomationBranch = 'yes' | 'no' | null;

export type TriggerConfig =
  | { type: 'list_joined'; list_id: string }
  | { type: 'tag_added'; list_id: string; tag: string }
  | { type: 'event'; list_id: string; name: string };
export interface SendEmailConfig {
  template_id: string;
  track_opens: boolean;
  track_clicks: boolean;
}
export type WaitConfig =
  | { mode: 'duration'; seconds: number }
  | { mode: 'until'; weekday?: number | null; time: string; timezone: string };
export type ConditionPredicate =
  | { type: 'opened_email'; node_id: string }
  | { type: 'clicked_link'; node_id: string; url: string | null }
  | { type: 'has_tag'; tag: string }
  | { type: 'on_list'; list_id: string }
  | { type: 'event_received'; name: string; within_seconds: number | null };
export interface ConditionConfig {
  predicate: ConditionPredicate;
}
export interface TagConfig {
  tag: string;
}

export type AutomationNodeConfig =
  | TriggerConfig
  | SendEmailConfig
  | WaitConfig
  | ConditionConfig
  | TagConfig
  | Record<string, never>;

export interface AutomationNode {
  id: string;
  kind: AutomationNodeKind;
  name?: string;
  config: AutomationNodeConfig;
}

export interface AutomationEdge {
  id: string;
  from: string;
  to: string;
  branch: AutomationBranch;
}

export interface AutomationGraph {
  nodes: AutomationNode[];
  edges: AutomationEdge[];
}

export interface Automation {
  id: string;
  business_id: string;
  tenant_root_id: string;
  name: string;
  description: string | null;
  status: AutomationStatus;
  allow_reenroll: boolean;
  active_version_id: string | null;
  draft_version_id: string | null;
  created_by_principal_id: string | null;
  created_at: string;
  updated_at: string;
}

export interface AutomationVersion {
  id: string;
  business_id: string;
  tenant_root_id: string;
  automation_id: string;
  number: number;
  status: AutomationVersionStatus;
  graph: AutomationGraph;
  trigger_kind: string | null;
  trigger_ref: string | null;
  activated_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface AutomationIssue {
  code: string;
  node_id?: string;
  edge_id?: string;
  message: string;
}

@Injectable({ providedIn: 'root' })
export class AutomationsService {
  private readonly http = inject(HttpClient);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}/mailing/automations`;
  }

  list(businessId: string, cursor?: string): Observable<Page<Automation>> {
    const params = cursor ? new HttpParams().set('cursor', cursor) : undefined;
    return this.http.get<Page<Automation>>(this.base(businessId), { params });
  }

  get(businessId: string, automationId: string): Observable<Automation> {
    return this.http.get<Automation>(`${this.base(businessId)}/${automationId}`);
  }

  create(
    businessId: string,
    input: { name: string; description?: string | null; allow_reenroll?: boolean },
  ): Observable<Automation> {
    return this.http.post<Automation>(this.base(businessId), input);
  }

  versions(businessId: string, automationId: string): Observable<{ items: AutomationVersion[] }> {
    return this.http.get<{ items: AutomationVersion[] }>(
      `${this.base(businessId)}/${automationId}/versions`,
    );
  }

  version(
    businessId: string,
    automationId: string,
    versionId: string,
  ): Observable<AutomationVersion> {
    return this.http.get<AutomationVersion>(
      `${this.base(businessId)}/${automationId}/versions/${versionId}`,
    );
  }

  putGraph(
    businessId: string,
    automationId: string,
    versionId: string,
    graph: AutomationGraph,
  ): Observable<AutomationVersion> {
    return this.http.put<AutomationVersion>(
      `${this.base(businessId)}/${automationId}/versions/${versionId}/graph`,
      graph,
    );
  }
}
