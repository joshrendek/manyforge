import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { CodexConnectComponent } from './codex-connect';

describe('CodexConnectComponent', () => {
  let fixture: ComponentFixture<CodexConnectComponent>;
  let http: HttpTestingController;

  function mount(): CodexConnectComponent {
    fixture = TestBed.createComponent(CodexConnectComponent);
    fixture.componentInstance.businessId = 'b1';
    fixture.detectChanges(); // triggers ngOnInit → models() fetch
    http.expectOne('/api/v1/businesses/b1/agents/models').flush({
      items: [
        { provider: 'openai_codex', model_id: 'gpt-5-codex' },
        { provider: 'anthropic', model_id: 'claude-opus-4-8' },
      ],
    });
    fixture.detectChanges();
    return fixture.componentInstance;
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [CodexConnectComponent],
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    http = TestBed.inject(HttpTestingController);
  });

  it('lists only openai_codex models in the picker', () => {
    const c = mount();
    expect(c.codexModels().map((m) => m.model_id)).toEqual(['gpt-5-codex']);
  });

  it('startDevice posts the body and moves to the authorizing phase showing the user code', () => {
    const c = mount();
    c.model.set('gpt-5-codex');
    c.startDevice();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start');
    expect(req.request.body).toEqual(expect.objectContaining({ default_model: 'gpt-5-codex' }));
    req.flush({ pending_id: 'p1', user_code: 'ABCD-1234', verification_uri: 'https://x', verification_uri_complete: 'https://x?c=ABCD', interval: 5, expires_in: 900 });
    fixture.detectChanges();
    expect(c.phase()).toBe('authorizing');
    expect(fixture.nativeElement.querySelector('[data-testid="codex-user-code"]')?.textContent).toContain('ABCD-1234');
  });

  it('pollOnce emits connected with the credential id when approved', () => {
    const c = mount();
    c.model.set('gpt-5-codex');
    c.startDevice();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start').flush({ pending_id: 'p1', user_code: 'X', verification_uri: 'u', verification_uri_complete: 'u', interval: 5, expires_in: 900 });
    let emitted = '';
    c.connected.subscribe((id) => (emitted = id));
    c.pollOnce();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/p1/status').flush({ status: 'approved', credential_id: 'cred9' });
    expect(emitted).toBe('cred9');
  });

  it('pollOnce moves to the expired phase when the code expires', () => {
    const c = mount();
    c.model.set('gpt-5-codex');
    c.startDevice();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start').flush({ pending_id: 'p1', user_code: 'X', verification_uri: 'u', verification_uri_complete: 'u', interval: 5, expires_in: 900 });
    c.pollOnce();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/p1/status').flush({ status: 'expired' });
    expect(c.phase()).toBe('expired');
  });

  it('submitPaste exchanges the pasted redirect URL and emits connected', () => {
    const c = mount();
    c.model.set('gpt-5-codex');
    c.startPaste();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/start').flush({ pending_id: 'p1', authorize_url: 'https://auth' });
    c.pasteUrl = 'http://localhost:1455/auth/callback?code=z&state=p1';
    let emitted = '';
    c.connected.subscribe((id) => (emitted = id));
    c.submitPaste();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/exchange').flush({ status: 'approved', credential_id: 'cred9' });
    expect(emitted).toBe('cred9');
  });

  it('moves to the expired phase when the device flow is denied', () => {
    const c = mount();
    c.model.set('gpt-5-codex');
    c.startDevice();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start').flush({ pending_id: 'p1', user_code: 'X', verification_uri: 'u', verification_uri_complete: 'u', interval: 5, expires_in: 900 });
    c.pollOnce();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/p1/status').flush({ status: 'denied' });
    expect(c.phase()).toBe('expired');
  });

  it('stops polling and surfaces an error when the status poll returns a 4xx', () => {
    const c = mount();
    c.model.set('gpt-5-codex');
    c.startDevice();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start').flush({ pending_id: 'p1', user_code: 'X', verification_uri: 'u', verification_uri_complete: 'u', interval: 5, expires_in: 900 });
    c.pollOnce();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/p1/status').flush({ message: 'gone' }, { status: 404, statusText: 'Not Found' });
    expect(c.phase()).toBe('expired');
    expect(c.error()).not.toBe('');
  });

  it('renders an authorize link (no popup) for the paste fallback', () => {
    const openSpy = vi.spyOn(window, 'open');
    const c = mount();
    c.model.set('gpt-5-codex');
    c.startDevice();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start').flush({ pending_id: 'p1', user_code: 'X', verification_uri: 'u', verification_uri_complete: 'u', interval: 5, expires_in: 900 });
    fixture.detectChanges();
    c.startPaste();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/start').flush({ pending_id: 'p1', authorize_url: 'https://auth.openai.com/authorize?x=1' });
    fixture.detectChanges();
    const link = fixture.nativeElement.querySelector('[data-testid="codex-paste-open"]');
    expect(link?.getAttribute('href')).toBe('https://auth.openai.com/authorize?x=1');
    expect(openSpy).not.toHaveBeenCalled();
    openSpy.mockRestore();
  });

  it('stops the device poll once the user switches to the paste fallback', () => {
    const c = mount();
    c.model.set('gpt-5-codex');
    vi.useFakeTimers();
    try {
      c.startDevice();
      http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/start').flush({ pending_id: 'p1', user_code: 'X', verification_uri: 'u', verification_uri_complete: 'u', interval: 5, expires_in: 900 });
      vi.advanceTimersByTime(5000);
      http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/device/p1/status').flush({ status: 'pending' });
      c.startPaste();
      http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/start').flush({ pending_id: 'p2', authorize_url: 'https://auth.openai.com/authorize?x=1' });
      vi.advanceTimersByTime(20000);
      http.expectNone('/api/v1/businesses/b1/ai_credentials/codex/device/p1/status');
    } finally {
      vi.useRealTimers();
    }
  });
});
