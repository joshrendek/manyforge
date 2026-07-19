import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AICredentialsService } from './ai-credentials.service';

describe('AICredentialsService codex methods', () => {
  let svc: AICredentialsService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    svc = TestBed.inject(AICredentialsService);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  it('codexDeviceStart POSTs the connect body to /codex/device/start', () => {
    let ok = false;
    svc.codexDeviceStart('b1', { default_model: 'gpt-5-codex', max_concurrent_lanes: 4 }).subscribe(() => (ok = true));
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual(expect.objectContaining({ default_model: 'gpt-5-codex' }));
    req.flush({ pending_id: 'p1', user_code: 'ABCD-1234', verification_uri: 'https://x', verification_uri_complete: 'https://x?c=ABCD-1234', interval: 5, expires_in: 900 });
    expect(ok).toBe(true);
  });

  it('codexDeviceStatus GETs the pending status', () => {
    let status = '';
    svc.codexDeviceStatus('b1', 'p1').subscribe((s) => (status = s.status));
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/p1/status');
    expect(req.request.method).toBe('GET');
    req.flush({ status: 'approved', credential_id: 'cred9' });
    expect(status).toBe('approved');
  });

  it('codexPKCEExchange POSTs pending_id + redirect_url to /codex/pkce/exchange', () => {
    svc.codexPKCEExchange('b1', 'p1', 'http://localhost:1455/auth/callback?code=z&state=p1').subscribe();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/exchange');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ pending_id: 'p1', redirect_url: 'http://localhost:1455/auth/callback?code=z&state=p1' });
    req.flush({ status: 'approved', credential_id: 'cred9' });
  });

  it('codexPKCEStart POSTs the connect body to /codex/pkce/start', () => {
    let url = '';
    svc.codexPKCEStart('b1', { default_model: 'gpt-5-codex', max_concurrent_lanes: 4 }).subscribe((r) => (url = r.authorize_url));
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/start');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual(expect.objectContaining({ default_model: 'gpt-5-codex' }));
    req.flush({ pending_id: 'p1', authorize_url: 'https://auth.openai.com/authorize?x=1' });
    expect(url).toBe('https://auth.openai.com/authorize?x=1');
  });
});
