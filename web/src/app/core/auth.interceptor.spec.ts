import { HttpClient, provideHttpClient, withInterceptors } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { authInterceptor } from './auth.interceptor';

// The interceptor is exercised against a real HttpClient + mock backend, so we
// test the actual request/refresh/retry behaviour rather than a mock of it.
describe('authInterceptor refresh-on-401', () => {
  let http: HttpClient;
  let mock: HttpTestingController;

  beforeEach(() => {
    localStorage.clear();
    localStorage.setItem('mf_access', 'old-access');
    localStorage.setItem('mf_refresh', 'refresh-1');
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(withInterceptors([authInterceptor])),
        provideHttpClientTesting(),
        // Mirror production: a matchable /login route so the refresh-failure
        // redirect resolves instead of rejecting (NG04002) in the test env.
        provideRouter([{ path: 'login', children: [] }]),
      ],
    });
    http = TestBed.inject(HttpClient);
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => mock.verify());

  it('attaches the bearer access token', () => {
    http.get('/api/v1/me').subscribe();
    const req = mock.expectOne('/api/v1/me');
    expect(req.request.headers.get('Authorization')).toBe('Bearer old-access');
    req.flush({});
  });

  it('on 401, refreshes then retries the original request once with the new token', () => {
    let result: unknown;
    http.get('/api/v1/businesses').subscribe((r) => (result = r));

    const first = mock.expectOne('/api/v1/businesses');
    expect(first.request.headers.get('Authorization')).toBe('Bearer old-access');
    first.flush(null, { status: 401, statusText: 'Unauthorized' });

    const refresh = mock.expectOne('/api/v1/auth/refresh');
    expect(refresh.request.body).toEqual({ refresh_token: 'refresh-1' });
    refresh.flush({ access_token: 'new-access', refresh_token: 'refresh-2', expires_in: 900 });

    const retry = mock.expectOne('/api/v1/businesses');
    expect(retry.request.headers.get('Authorization')).toBe('Bearer new-access');
    retry.flush({ items: [] });

    expect(result).toEqual({ items: [] });
    expect(localStorage.getItem('mf_access')).toBe('new-access');
    expect(localStorage.getItem('mf_refresh')).toBe('refresh-2');
  });

  it('refreshes only once for concurrent 401s (single-flight)', () => {
    http.get('/api/v1/a').subscribe();
    http.get('/api/v1/b').subscribe();

    const initial = mock.match((r) => r.url === '/api/v1/a' || r.url === '/api/v1/b');
    expect(initial.length).toBe(2);
    initial.forEach((r) => r.flush(null, { status: 401, statusText: 'Unauthorized' }));

    const refreshes = mock.match('/api/v1/auth/refresh');
    expect(refreshes.length).toBe(1);
    refreshes[0].flush({ access_token: 'new-access', refresh_token: 'r2', expires_in: 900 });

    const retries = mock.match((r) => r.url === '/api/v1/a' || r.url === '/api/v1/b');
    expect(retries.length).toBe(2);
    retries.forEach((r) => r.flush({}));
  });

  it('does not attempt refresh for auth endpoints — a login 401 propagates', () => {
    let errStatus = 0;
    http.post('/api/v1/auth/login', {}).subscribe({ error: (e) => (errStatus = e.status) });
    mock.expectOne('/api/v1/auth/login').flush(null, { status: 401, statusText: 'Unauthorized' });
    mock.expectNone('/api/v1/auth/refresh');
    expect(errStatus).toBe(401);
  });

  it('clears the session when the refresh itself fails', () => {
    let errStatus = 0;
    http.get('/api/v1/me').subscribe({ error: (e) => (errStatus = e.status) });
    mock.expectOne('/api/v1/me').flush(null, { status: 401, statusText: 'Unauthorized' });
    mock.expectOne('/api/v1/auth/refresh').flush(null, { status: 401, statusText: 'Unauthorized' });
    expect(errStatus).toBe(401);
    expect(localStorage.getItem('mf_access')).toBeNull();
    expect(localStorage.getItem('mf_refresh')).toBeNull();
  });
});
