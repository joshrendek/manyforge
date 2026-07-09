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

  // huggingface targets the HF Inference Providers router, which has one canonical endpoint —
  // so it prefills like openrouter and base_url is optional (mirrors ai.DefaultBaseURL).
  it('prefills the HuggingFace router base URL and posts a partner-pinned model id', () => {
    const c = fixture.componentInstance;
    c.onProviderChange('huggingface');
    expect(c.baseUrlRequired()).toBe(false);
    expect(c.baseUrl).toBe('https://router.huggingface.co/v1');

    c.apiKey = 'hf_test';
    c.defaultModel = 'zai-org/GLM-5.2:fireworks-ai';
    expect(c.valid()).toBe(true);
    c.submit();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials');
    expect(req.request.body.provider).toBe('huggingface');
    expect(req.request.body.base_url).toBe('https://router.huggingface.co/v1');
    // The ":partner" suffix pins pricing and routing; it must survive the form untouched.
    expect(req.request.body.default_model).toBe('zai-org/GLM-5.2:fireworks-ai');
    req.flush({});
  });

  // Switching between two prefilled providers must SWAP the default, not strand the old one.
  it('swaps the prefilled base URL when moving between prefilled providers', () => {
    const c = fixture.componentInstance;
    c.onProviderChange('openrouter');
    expect(c.baseUrl).toBe('https://openrouter.ai/api/v1');
    c.onProviderChange('huggingface');
    expect(c.baseUrl).toBe('https://router.huggingface.co/v1');
    c.onProviderChange('openai');
    expect(c.baseUrl).toBe('');
  });

  // A base_url the user typed themselves is never clobbered by a provider switch.
  it('never clobbers a user-typed base URL', () => {
    const c = fixture.componentInstance;
    c.baseUrl = 'https://my-gateway.internal/v1';
    c.onProviderChange('huggingface');
    expect(c.baseUrl).toBe('https://my-gateway.internal/v1');
  });

  // Only providers ai.DefaultBaseURL knows about may omit base_url.
  it('requires a base URL exactly for the providers with no server-side default', () => {
    const c = fixture.componentInstance;
    for (const p of ['anthropic', 'openrouter', 'huggingface'] as const) {
      c.provider.set(p);
      expect(c.baseUrlRequired()).toBe(false);
    }
    for (const p of ['openai', 'ollama', 'vllm'] as const) {
      c.provider.set(p);
      expect(c.baseUrlRequired()).toBe(true);
    }
  });
});
