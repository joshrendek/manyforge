import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

export interface MCPServer {
  id: string;
  business_id: string;
  name: string;
  url: string;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface CreateMCPServerBody {
  name: string;
  url: string;
  auth_token?: string; // write-only; never returned
}

export interface UpdateMCPServerBody {
  name?: string;
  url?: string;
  enabled?: boolean;
  auth_token?: string; // omit to keep current; "" to clear
}

export type ToolEffect = 'read' | 'reversible' | 'external';

export interface DiscoveredTool {
  name: string;
  description: string;
  effect: ToolEffect;
}

export interface DiscoverToolsResp {
  reachable: boolean;
  tools: DiscoveredTool[];
}

@Injectable({ providedIn: 'root' })
export class McpService {
  private http = inject(HttpClient);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}/mcp_servers`;
  }

  list(businessId: string): Observable<{ items: MCPServer[] }> {
    return this.http.get<{ items: MCPServer[] }>(this.base(businessId));
  }
  create(businessId: string, body: CreateMCPServerBody): Observable<MCPServer> {
    return this.http.post<MCPServer>(this.base(businessId), body);
  }
  update(businessId: string, id: string, body: UpdateMCPServerBody): Observable<MCPServer> {
    return this.http.patch<MCPServer>(`${this.base(businessId)}/${id}`, body);
  }
  remove(businessId: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/${id}`);
  }
  discoverTools(businessId: string, serverId: string): Observable<DiscoverToolsResp> {
    return this.http.get<DiscoverToolsResp>(`${this.base(businessId)}/${serverId}/tools`);
  }
  setPolicy(
    businessId: string,
    serverId: string,
    toolName: string,
    effect: 'read' | 'reversible',
  ): Observable<{ tool_name: string; effect: string }> {
    return this.http.put<{ tool_name: string; effect: string }>(
      `${this.base(businessId)}/${serverId}/tool_policies/${encodeURIComponent(toolName)}`,
      { effect },
    );
  }
  clearPolicy(businessId: string, serverId: string, toolName: string): Observable<void> {
    return this.http.delete<void>(
      `${this.base(businessId)}/${serverId}/tool_policies/${encodeURIComponent(toolName)}`,
    );
  }
}
