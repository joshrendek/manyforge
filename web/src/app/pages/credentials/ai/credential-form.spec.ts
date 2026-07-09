import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { CredentialFormComponent } from './credential-form';

describe('CredentialFormComponent', () => {
  let fixture: ComponentFixture<CredentialFormComponent>;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [CredentialFormComponent],
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    fixture = TestBed.createComponent(CredentialFormComponent);
    fixture.componentInstance.businessId = 'b1';
    fixture.detectChanges();
    http = TestBed.inject(HttpTestingController);
  });

  it('emits a create payload with provider, api_key, default_model', () => {
    const c = fixture.componentInstance;
    c.provider.set('anthropic');
    c.apiKey = 'sk-ant-xyz';
    c.defaultModel = 'claude-opus-4-8';
    let saved = false;
    c.saved.subscribe(() => (saved = true));

    c.submit();

    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual(
      expect.objectContaining({ provider: 'anthropic', api_key: 'sk-ant-xyz', default_model: 'claude-opus-4-8' }),
    );
    req.flush({
      id: 'cred1', business_id: 'b1', provider: 'anthropic', base_url: '', default_model: 'claude-opus-4-8',
      allow_private_base_url: false, created_at: '', updated_at: '',
    });
    expect(saved).toBe(true);
  });

  it('sends the endpoint max_concurrent_lanes in the create payload', () => {
    const c = fixture.componentInstance;
    c.provider.set('vllm');
    c.apiKey = 'lmstudio';
    c.defaultModel = 'ornith-1.0-9b';
    c.baseUrl = 'http://192.168.2.241:1234/v1';
    c.maxConcurrentLanes = 1; // single-GPU self-host
    c.submit();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials');
    expect(req.request.body).toEqual(expect.objectContaining({ max_concurrent_lanes: 1 }));
    req.flush({
      id: 'c1', business_id: 'b1', provider: 'vllm', base_url: 'http://192.168.2.241:1234/v1',
      default_model: 'ornith-1.0-9b', allow_private_base_url: true, max_concurrent_lanes: 1, created_at: '', updated_at: '',
    });
  });

  it('prefills the OpenRouter base URL when openrouter is selected', () => {
    const c = fixture.componentInstance;
    expect(c.baseUrl).toBe('');
    c.onProviderChange('openrouter');
    expect(c.provider()).toBe('openrouter');
    expect(c.baseUrl).toBe('https://openrouter.ai/api/v1');
    // switching away to a provider that needs an explicit base_url clears the auto-filled default
    c.onProviderChange('openai');
    expect(c.baseUrl).toBe('');
  });

  it('maps a 400 to a "Rejected:" message', () => {
    const c = fixture.componentInstance;
    c.provider.set('openai');
    c.apiKey = 'k';
    c.defaultModel = 'gpt-5';
    c.baseUrl = 'https://api.openai.com/v1'; // openai has no server-side default
    c.submit();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials');
    req.flush({ message: 'model not available' }, { status: 400, statusText: 'Bad Request' });
    expect(c.error()).toContain('Rejected: model not available');
  });

  // A huggingface credential points at the operator's own ZeroGPU Space, so base_url is
  // required — there is no per-user default to fall back to (mirrors credential.go validate()).
  it('requires a base URL for huggingface and blocks submit without one', () => {
    const c = fixture.componentInstance;
    c.onProviderChange('huggingface');
    c.apiKey = 'hf_test';
    c.defaultModel = 'Qwen/Qwen3-14B';

    expect(c.baseUrlRequired()).toBe(true);
    expect(c.baseUrl).toBe(''); // nothing is prefilled: the Space host is per-user
    expect(c.valid()).toBe(false);
    c.submit();
    http.expectNone('/api/v1/businesses/b1/ai_credentials');

    c.baseUrl = 'https://josh-reviewbot.hf.space/v1';
    expect(c.valid()).toBe(true);
    c.submit();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials');
    expect(req.request.body.provider).toBe('huggingface');
    expect(req.request.body.base_url).toBe('https://josh-reviewbot.hf.space/v1');
    req.flush({});
  });

  // Anthropic and OpenRouter are the only providers the server defaults a base_url for.
  it('treats base URL as optional only for anthropic and openrouter', () => {
    const c = fixture.componentInstance;
    for (const p of ['anthropic', 'openrouter'] as const) {
      c.provider.set(p);
      expect(c.baseUrlRequired()).toBe(false);
    }
    for (const p of ['openai', 'ollama', 'vllm', 'huggingface'] as const) {
      c.provider.set(p);
      expect(c.baseUrlRequired()).toBe(true);
    }
  });
});
