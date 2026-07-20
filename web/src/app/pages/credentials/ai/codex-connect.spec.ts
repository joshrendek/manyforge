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
        { provider: 'openai_codex', model_id: 'gpt-5.6-sol' },
        { provider: 'openai_codex', model_id: 'gpt-5.6-terra' },
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

  it('lists only openai_codex models and pre-selects the first', () => {
    const c = mount();
    expect(c.codexModels().map((m) => m.model_id)).toEqual(['gpt-5.6-sol', 'gpt-5.6-terra']);
    expect(c.model()).toBe('gpt-5.6-sol');
  });

  it('startSignin posts the PKCE body and moves to the signin phase with an authorize link', () => {
    const c = mount();
    c.startSignin();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/start');
    expect(req.request.body).toEqual(expect.objectContaining({ default_model: 'gpt-5.6-sol' }));
    req.flush({ pending_id: 'p1', authorize_url: 'https://auth.openai.com/oauth/authorize?x=1' });
    fixture.detectChanges();
    expect(c.phase()).toBe('signin');
    const link = fixture.nativeElement.querySelector('[data-testid="codex-open"]');
    expect(link?.getAttribute('href')).toBe('https://auth.openai.com/oauth/authorize?x=1');
    expect(link?.getAttribute('target')).toBe('_blank');
  });

  it('opens no popup — the authorize step is a link, not window.open', () => {
    const openSpy = vi.spyOn(window, 'open');
    const c = mount();
    c.startSignin();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/start').flush({ pending_id: 'p1', authorize_url: 'https://auth' });
    fixture.detectChanges();
    expect(openSpy).not.toHaveBeenCalled();
    openSpy.mockRestore();
  });

  it('submitPaste exchanges the pasted redirect URL and emits connected', () => {
    const c = mount();
    c.startSignin();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/start').flush({ pending_id: 'p1', authorize_url: 'https://auth' });
    c.pasteUrl = 'http://localhost:1455/auth/callback?code=z&state=p1';
    let emitted = '';
    c.connected.subscribe((id) => (emitted = id));
    c.submitPaste();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/exchange');
    expect(req.request.body).toEqual({ pending_id: 'p1', redirect_url: 'http://localhost:1455/auth/callback?code=z&state=p1' });
    req.flush({ status: 'approved', credential_id: 'cred9' });
    expect(emitted).toBe('cred9');
  });

  it('surfaces an error and stays on the signin phase when the exchange 404s (expired pending)', () => {
    const c = mount();
    c.startSignin();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/start').flush({ pending_id: 'p1', authorize_url: 'https://auth' });
    c.pasteUrl = 'http://localhost:1455/auth/callback?code=z&state=p1';
    c.submitPaste();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/exchange').flush({ message: 'gone' }, { status: 404, statusText: 'Not Found' });
    fixture.detectChanges();
    expect(c.phase()).toBe('signin');
    expect(c.error()).not.toBe('');
  });

  it('shows an error (no connect) when the exchange reports denied', () => {
    const c = mount();
    c.startSignin();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/start').flush({ pending_id: 'p1', authorize_url: 'https://auth' });
    c.pasteUrl = 'http://localhost:1455/auth/callback?code=z&state=p1';
    let emitted = '';
    c.connected.subscribe((id) => (emitted = id));
    c.submitPaste();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/exchange').flush({ status: 'denied' });
    expect(emitted).toBe('');
    expect(c.error()).not.toBe('');
  });

  it('reset returns to the configure phase and clears the paste field', () => {
    const c = mount();
    c.startSignin();
    http.expectOne('/api/v1/businesses/b1/ai_credentials/codex/pkce/start').flush({ pending_id: 'p1', authorize_url: 'https://auth' });
    c.pasteUrl = 'x';
    c.reset();
    expect(c.phase()).toBe('configure');
    expect(c.pasteUrl).toBe('');
  });
});
