import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

// The single source of truth for provider names on the client. Mirrors the ai_provider PG
// enum (db/schema.sql) and agents.knownProviders — keep them in sync.
// 'huggingface' is the HF Inference Providers router (router.huggingface.co).
export type AIProvider = 'anthropic' | 'openai' | 'ollama' | 'vllm' | 'openrouter' | 'huggingface' | 'openai_codex';

// Read shape: no api_key — the secret is write-only.
export interface AICredential {
  id: string;
  business_id: string;
  provider: AIProvider;
  base_url: string;
  default_model: string;
  allow_private_base_url: boolean;
  max_concurrent_lanes: number;
  created_at: string;
  updated_at: string;
  // openai_codex-only connection health (omitted/empty for other providers; never secret-bearing).
  chatgpt_plan?: string;
  connection_status?: 'connected' | 'disconnected';
  oauth_access_expiry?: string;
}

export interface CreateAICredentialBody {
  provider: AIProvider;
  api_key: string;
  default_model: string;
  base_url?: string;
  allow_private_base_url?: boolean;
  max_concurrent_lanes?: number;
}

export interface UpdateAICredentialBody {
  default_model?: string;
  max_concurrent_lanes?: number;
}

// Request body for the codex PKCE start endpoint.
export interface CodexConnectBody {
  default_model: string;
  base_url?: string;
  max_concurrent_lanes?: number;
}
// Response from starting a PKCE (paste-the-redirect-URL) flow: where to send the user.
export interface CodexPKCEStart {
  pending_id: string;
  authorize_url: string;
}
// Poll/exchange result for either flow; credential_id is set once status is 'approved'.
export interface CodexConnectStatus {
  status: 'pending' | 'approved' | 'expired' | 'denied';
  credential_id?: string;
}

// AICredentialsService talks to the agents.configure-gated credential API.
@Injectable({ providedIn: 'root' })
export class AICredentialsService {
  private http = inject(HttpClient);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}/ai_credentials`;
  }

  list(businessId: string): Observable<{ items: AICredential[] }> {
    return this.http.get<{ items: AICredential[] }>(this.base(businessId));
  }
  create(businessId: string, body: CreateAICredentialBody): Observable<AICredential> {
    return this.http.post<AICredential>(this.base(businessId), body);
  }
  remove(businessId: string, id: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/${id}`);
  }
  update(businessId: string, id: string, body: UpdateAICredentialBody): Observable<AICredential> {
    return this.http.patch<AICredential>(`${this.base(businessId)}/${id}`, body);
  }
  // liveCodexModels returns the connected account's live per-plan Codex models (shaped like
  // /agents/models). Empty when the feature is off / not connected / upstream fails — callers then
  // fall back to the static catalog.
  liveCodexModels(businessId: string): Observable<{ items: { provider: string; model_id: string }[] }> {
    return this.http.get<{ items: { provider: string; model_id: string }[] }>(`${this.base(businessId)}/codex/models`);
  }
  codexPKCEStart(businessId: string, body: CodexConnectBody): Observable<CodexPKCEStart> {
    return this.http.post<CodexPKCEStart>(`${this.base(businessId)}/codex/pkce/start`, body);
  }
  codexPKCEExchange(businessId: string, pendingId: string, redirectUrl: string): Observable<CodexConnectStatus> {
    return this.http.post<CodexConnectStatus>(`${this.base(businessId)}/codex/pkce/exchange`, {
      pending_id: pendingId,
      redirect_url: redirectUrl,
    });
  }
}
