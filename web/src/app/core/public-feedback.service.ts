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
  // Server truth for the caller's own vote, scoped to the voter_identity passed on the list
  // call (device id, namespaced server-side into the a: tier) — not just optimistic local state.
  viewer_voted: boolean;
  // Set when the post's author identity was cryptographically verified via the ingest key's
  // write-once secret, rather than just claimed by the client.
  identity_verified: boolean;
}

@Injectable({ providedIn: 'root' })
export class PublicFeedbackService {
  private http = inject(HttpClient);

  private base(key: string): string {
    return `/api/v1/feedback/public/${encodeURIComponent(key)}`;
  }

  // voterIdentity (the portal's per-browser device id) is echoed back as viewer_voted on each
  // post so the vote button can reflect server truth on load, not just a local vote cache.
  listPosts(key: string, voterIdentity?: string): Observable<{ items: PublicPost[] }> {
    const params = voterIdentity ? { voter_identity: voterIdentity } : undefined;
    return this.http.get<{ items: PublicPost[] }>(`${this.base(key)}/posts`, { params });
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
