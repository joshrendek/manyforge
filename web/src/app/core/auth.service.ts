import { HttpClient } from '@angular/common/http';
import { Injectable, computed, inject, signal } from '@angular/core';
import { Observable, of, tap } from 'rxjs';

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
