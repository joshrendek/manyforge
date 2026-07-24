import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';

// PublicFeedbackService is the UNAUTHENTICATED counterpart to FeedbackService: it calls the
// principal-less public ingress (/api/v1/feedback/public/{key}/…), authenticated by a per-board
// publishable key carried in the URL (the same endpoints the Apple/Android SDKs use). A 401
// means an unknown/revoked key or a private board — the auth interceptor is told to skip its
// refresh/redirect for this path, so the error surfaces to the portal component instead.
export interface PublicPost {
  id: string;
  title: string;
  body?: string | null;
  status: string;
  vote_count: number;
  created_at: string;
}

@Injectable({ providedIn: 'root' })
export class PublicFeedbackService {
  private http = inject(HttpClient);

  private base(key: string): string {
    return `/api/v1/feedback/public/${encodeURIComponent(key)}`;
  }

  listPosts(key: string): Observable<{ items: PublicPost[] }> {
    return this.http.get<{ items: PublicPost[] }>(`${this.base(key)}/posts`);
  }

  submit(
    key: string,
    body: { title: string; body?: string; author_identity?: string },
  ): Observable<{ id: string; title: string; status: string; vote_count: number }> {
    return this.http.post<{ id: string; title: string; status: string; vote_count: number }>(
      `${this.base(key)}/posts`,
      body,
    );
  }

  vote(
    key: string,
    postId: string,
    voterIdentity: string,
  ): Observable<{ voted: boolean; vote_count: number }> {
    return this.http.post<{ voted: boolean; vote_count: number }>(
      `${this.base(key)}/posts/${postId}/votes`,
      {
        voter_identity: voterIdentity,
      },
    );
  }
}
