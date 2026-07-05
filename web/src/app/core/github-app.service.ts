import { Injectable, inject } from '@angular/core';
import { HttpClient } from '@angular/common/http';
import { Observable } from 'rxjs';

// GithubAppService talks to the Task 6 backend endpoints that finish the two
// GitHub App browser redirects: manifest conversion (creating the App) and
// installation linking (attaching an install to a business). The Bearer token
// is auto-attached by the functional authInterceptor — no manual auth headers.
@Injectable({ providedIn: 'root' })
export class GithubAppService {
  private http = inject(HttpClient);
  convertManifest(body: { code: string; state: string }): Observable<{ slug: string }> {
    return this.http.post<{ slug: string }>('/api/v1/github/app/manifest/convert', body);
  }
  linkInstallation(body: {
    code: string;
    installation_id: string;
    state: string;
  }): Observable<{ linked: boolean }> {
    return this.http.post<{ linked: boolean }>('/api/v1/github/app/installations/link', body);
  }

  // getManifest fetches the App-creation manifest + signed single-use "manifest"
  // state (operator-gated: 401 unauthenticated, 404 authenticated non-operator).
  // The SPA POSTs the returned manifest straight to action_url — see
  // github-app-settings.ts, which builds that form via the DOM rather than a
  // template binding so the JSON manifest string is never HTML-escaped.
  getManifest(): Observable<{ action_url: string; manifest: string; state: string }> {
    return this.http.get<{ action_url: string; manifest: string; state: string }>(
      '/api/v1/github/app/manifest',
    );
  }

  // getInstallUrl mints a GitHub App installation URL carrying a signed
  // single-use "link" state for the given business + agent (behind
  // connectors.manage; 404 if the App isn't created yet or the caller lacks
  // permission — no oracle).
  getInstallUrl(businessId: string, agentId: string): Observable<{ install_url: string }> {
    return this.http.get<{ install_url: string }>(
      `/api/v1/businesses/${businessId}/github/app/install-url`,
      { params: { agent_id: agentId } },
    );
  }
}
