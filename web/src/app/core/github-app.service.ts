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
}
