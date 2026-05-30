import { HttpClient } from '@angular/common/http';
import { Injectable, computed, inject, signal } from '@angular/core';
import { Observable, finalize, map, of, shareReplay, tap, throwError } from 'rxjs';

const ACCESS = 'mf_access';
const REFRESH = 'mf_refresh';

export interface TokenPair {
  access_token: string;
  refresh_token: string;
  expires_in: number;
}

export interface Profile {
  id: string;
  email: string;
  display_name: string;
  email_verified: boolean;
  status: string;
}

@Injectable({ providedIn: 'root' })
export class AuthService {
  private http = inject(HttpClient);

  readonly accessToken = signal<string | null>(localStorage.getItem(ACCESS));
  readonly isAuthenticated = computed(() => !!this.accessToken());

  // Shared in-flight refresh so a burst of 401s triggers exactly one /auth/refresh.
  private refresh$: Observable<string> | null = null;

  signup(email: string, displayName: string, password: string): Observable<unknown> {
    return this.http.post('/api/v1/auth/signup', { email, display_name: displayName, password });
  }

  verify(token: string): Observable<unknown> {
    return this.http.post('/api/v1/auth/verify-email', { token });
  }

  login(email: string, password: string): Observable<TokenPair> {
    return this.http
      .post<TokenPair>('/api/v1/auth/login', { email, password })
      .pipe(tap((r) => this.setTokens(r.access_token, r.refresh_token)));
  }

  logout(): Observable<unknown> {
    const refresh = localStorage.getItem(REFRESH);
    this.clear();
    return refresh ? this.http.post('/api/v1/auth/logout', { refresh_token: refresh }) : of(null);
  }

  me(): Observable<Profile> {
    return this.http.get<Profile>('/api/v1/me');
  }

  // refreshAccessToken rotates the token pair and returns the new access token.
  // Concurrent callers share one HTTP request (shareReplay); the slot is cleared
  // on completion or error so the next 401 can refresh again.
  refreshAccessToken(): Observable<string> {
    if (this.refresh$) {
      return this.refresh$;
    }
    const refresh = localStorage.getItem(REFRESH);
    if (!refresh) {
      return throwError(() => new Error('no refresh token'));
    }
    this.refresh$ = this.http.post<TokenPair>('/api/v1/auth/refresh', { refresh_token: refresh }).pipe(
      tap((r) => this.setTokens(r.access_token, r.refresh_token)),
      map((r) => r.access_token),
      shareReplay(1),
      finalize(() => (this.refresh$ = null)),
    );
    return this.refresh$;
  }

  // clearSession drops local credentials without calling the API (used when the
  // server has already rejected us, e.g. a failed refresh).
  clearSession(): void {
    this.clear();
  }

  private setTokens(access: string, refresh: string): void {
    localStorage.setItem(ACCESS, access);
    localStorage.setItem(REFRESH, refresh);
    this.accessToken.set(access);
  }

  private clear(): void {
    localStorage.removeItem(ACCESS);
    localStorage.removeItem(REFRESH);
    this.accessToken.set(null);
  }
}
