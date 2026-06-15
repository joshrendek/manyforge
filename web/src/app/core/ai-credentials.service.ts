import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

export type AIProvider = 'anthropic' | 'openai' | 'ollama' | 'vllm';

// Read shape: no api_key — the secret is write-only.
export interface AICredential {
  id: string;
  business_id: string;
  provider: AIProvider;
  base_url: string;
  default_model: string;
  allow_private_base_url: boolean;
  created_at: string;
  updated_at: string;
}

export interface CreateAICredentialBody {
  provider: AIProvider;
  api_key: string;
  default_model: string;
  base_url?: string;
  allow_private_base_url?: boolean;
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
}
