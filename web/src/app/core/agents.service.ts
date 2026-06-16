import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';
import { AIProvider } from './ai-credentials.service';
import { McpService, MCPServer } from './mcp.service';

export interface Agent {
  id: string;
  business_id: string;
  principal_id: string;
  name: string;
  provider: AIProvider;
  model: string;
  system_prompt: string;
  allowed_tools: string[];
  autonomy_mode: number;
  enabled: boolean;
  monthly_budget_cents: number;
  allowed_mcp_servers: string[];
  retriage_on_reply: boolean;
  created_at: string;
  updated_at: string;
}

export interface CreateAgentBody {
  name: string;
  provider: AIProvider;
  model: string;
  system_prompt: string;
  allowed_tools: string[];
  autonomy_mode: number;
  enabled: boolean;
  monthly_budget_cents: number;
  allowed_mcp_servers: string[];
  retriage_on_reply: boolean;
}

export type UpdateAgentBody = Partial<Omit<CreateAgentBody, 'provider'>>;

export interface ToolDescriptor {
  name: string;
  description: string;
  effect: 'read' | 'reversible' | 'external' | 'irreversible' | 'unknown';
  required_perm?: string;
}

export interface ModelDescriptor {
  provider: string;
  model_id: string;
}

export interface AgentRun {
  id: string;
  agent_id: string;
  trigger: string;
  status: 'queued' | 'running' | 'awaiting_approval' | 'succeeded' | 'failed';
  tokens_in: number;
  tokens_out: number;
  cost_cents: number;
  correlation_id: string;
  error?: string;
}

// Re-export MCPServer for consumers who need it alongside AgentsService.
export type { MCPServer };

@Injectable({ providedIn: 'root' })
export class AgentsService {
  private http = inject(HttpClient);
  private mcpSvc = inject(McpService);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}/agents`;
  }

  list(businessId: string): Observable<{ items: Agent[] }> {
    return this.http.get<{ items: Agent[] }>(this.base(businessId));
  }

  create(businessId: string, body: CreateAgentBody): Observable<Agent> {
    return this.http.post<Agent>(this.base(businessId), body);
  }

  update(businessId: string, id: string, body: UpdateAgentBody): Observable<Agent> {
    return this.http.patch<Agent>(`${this.base(businessId)}/${id}`, body);
  }

  remove(businessId: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/${id}`);
  }

  tools(businessId: string): Observable<{ items: ToolDescriptor[] }> {
    return this.http.get<{ items: ToolDescriptor[] }>(`${this.base(businessId)}/tools`);
  }

  models(businessId: string): Observable<{ items: ModelDescriptor[] }> {
    return this.http.get<{ items: ModelDescriptor[] }>(`${this.base(businessId)}/models`);
  }

  /** Returns the business's MCP servers for the agent form's server picker. */
  mcpServers(businessId: string): Observable<{ items: MCPServer[] }> {
    return this.mcpSvc.list(businessId);
  }

  // run triggers a manual agent run; with a ticket target the agent acts on that
  // ticket. The backend runs it synchronously and returns the terminal run (202).
  run(businessId: string, agentId: string, body: { target_type: 'ticket'; target_id: string }): Observable<AgentRun> {
    return this.http.post<AgentRun>(`${this.base(businessId)}/${agentId}/runs`, body);
  }
}
