import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { Observable } from 'rxjs';
import { Page } from './ticket.service';

// Feedback / feature-request boards (spec 006). Reached through a business-scoped URL.
// Shapes mirror internal/feedback/handler.go response DTOs (snake_case) exactly.
export interface Board {
  id: string;
  business_id: string;
  tenant_root_id: string;
  slug: string;
  name: string;
  description?: string | null;
  is_public: boolean;
  created_at: string;
  updated_at: string;
}

export interface Post {
  id: string;
  business_id: string;
  tenant_root_id: string;
  board_id: string;
  title: string;
  body?: string | null;
  status: string;
  vote_count: number;
  author_kind: string;
  author_principal_id?: string | null;
  author_identity?: string | null;
  ticket_id?: string | null;
  created_at: string;
  updated_at: string;
}

// A publishable ingest key embedded in an Apple/Android SDK. publishable_key is PUBLIC
// (not a secret), so it is safe to display with a copy button — unlike a masked token.
export interface IngestKey {
  id: string;
  business_id: string;
  tenant_root_id: string;
  board_id: string;
  publishable_key: string;
  label?: string | null;
  status: string;
  created_at: string;
  revoked_at?: string | null;
}

@Injectable({ providedIn: 'root' })
export class FeedbackService {
  private http = inject(HttpClient);

  private base(businessId: string): string {
    return `/api/v1/businesses/${businessId}`;
  }

  listBoards(businessId: string, cursor?: string): Observable<Page<Board>> {
    const q = cursor ? `?cursor=${encodeURIComponent(cursor)}` : '';
    return this.http.get<Page<Board>>(`${this.base(businessId)}/feedback/boards${q}`);
  }

  getBoard(businessId: string, boardId: string): Observable<Board> {
    return this.http.get<Board>(`${this.base(businessId)}/feedback/boards/${boardId}`);
  }

  createBoard(
    businessId: string,
    body: { name: string; slug?: string; description?: string; is_public?: boolean },
  ): Observable<Board> {
    return this.http.post<Board>(`${this.base(businessId)}/feedback/boards`, body);
  }

  updateBoard(
    businessId: string,
    boardId: string,
    body: Partial<{ name: string; description: string; is_public: boolean }>,
  ): Observable<Board> {
    return this.http.patch<Board>(`${this.base(businessId)}/feedback/boards/${boardId}`, body);
  }

  listPosts(businessId: string, boardId: string, cursor?: string): Observable<Page<Post>> {
    const q = cursor ? `?cursor=${encodeURIComponent(cursor)}` : '';
    return this.http.get<Page<Post>>(
      `${this.base(businessId)}/feedback/boards/${boardId}/posts${q}`,
    );
  }

  createPost(
    businessId: string,
    boardId: string,
    body: { title: string; body?: string },
  ): Observable<Post> {
    return this.http.post<Post>(`${this.base(businessId)}/feedback/boards/${boardId}/posts`, body);
  }

  getPost(businessId: string, postId: string): Observable<Post> {
    return this.http.get<Post>(`${this.base(businessId)}/feedback/posts/${postId}`);
  }

  setPostStatus(businessId: string, postId: string, status: string): Observable<Post> {
    return this.http.patch<Post>(`${this.base(businessId)}/feedback/posts/${postId}`, { status });
  }

  deletePost(businessId: string, postId: string): Observable<void> {
    return this.http.delete<void>(`${this.base(businessId)}/feedback/posts/${postId}`);
  }

  votePost(businessId: string, postId: string): Observable<{ voted: boolean; vote_count: number }> {
    return this.http.post<{ voted: boolean; vote_count: number }>(
      `${this.base(businessId)}/feedback/posts/${postId}/vote`,
      {},
    );
  }

  convertPost(businessId: string, postId: string): Observable<{ ticket_id: string }> {
    return this.http.post<{ ticket_id: string }>(
      `${this.base(businessId)}/feedback/posts/${postId}/convert`,
      {},
    );
  }

  listKeys(businessId: string, boardId: string): Observable<{ items: IngestKey[] }> {
    return this.http.get<{ items: IngestKey[] }>(
      `${this.base(businessId)}/feedback/boards/${boardId}/keys`,
    );
  }

  createKey(businessId: string, boardId: string, body: { label?: string }): Observable<IngestKey> {
    return this.http.post<IngestKey>(
      `${this.base(businessId)}/feedback/boards/${boardId}/keys`,
      body,
    );
  }

  revokeKey(businessId: string, keyId: string): Observable<IngestKey> {
    return this.http.post<IngestKey>(`${this.base(businessId)}/feedback/keys/${keyId}/revoke`, {});
  }
}
