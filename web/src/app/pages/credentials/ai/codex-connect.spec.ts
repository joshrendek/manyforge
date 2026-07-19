import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
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
});
