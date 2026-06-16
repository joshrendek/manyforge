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
    c.submit();
    const req = http.expectOne('/api/v1/businesses/b1/ai_credentials');
    req.flush({ message: 'base_url not allowed' }, { status: 400, statusText: 'Bad Request' });
    expect(c.error()).toContain('Rejected: base_url not allowed');
  });
});
