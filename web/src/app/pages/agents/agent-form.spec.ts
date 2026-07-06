import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, TestRequest, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { AgentFormComponent } from './agent-form';

function flushMetadata(http: HttpTestingController) {
  http.expectOne('/api/v1/businesses/b1/agents/tools').flush({
    items: [{ name: 'read_ticket', description: 'read', effect: 'read', required_perm: 'tickets.read' }],
  });
  http.expectOne('/api/v1/businesses/b1/agents/models').flush({
    items: [
      { provider: 'anthropic', model_id: 'claude-opus-4-8' },
      { provider: 'openai', model_id: 'gpt-5' },
    ],
  });
  http.expectOne('/api/v1/businesses/b1/mcp_servers').flush({
    items: [{ id: 'm1', name: 'docs', url: 'https://x', enabled: true }],
  });
}

describe('AgentFormComponent', () => {
  let fixture: ComponentFixture<AgentFormComponent>;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      imports: [AgentFormComponent],
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    fixture = TestBed.createComponent(AgentFormComponent);
    fixture.componentInstance.businessId = 'b1';
    fixture.detectChanges(); // triggers ngOnInit metadata loads
    http = TestBed.inject(HttpTestingController);
    flushMetadata(http);
  });

  it('emits a create payload with cents-converted budget and selected tool', () => {
    const c = fixture.componentInstance;
    c.name = 'Triage Bot';
    c.provider.set('anthropic');
    c.model = 'claude-opus-4-8';
    c.systemPrompt = 'Be helpful';
    c.toggleTool('read_ticket');
    c.budgetDollars = 25; // → 2500 cents
    let saved = false;
    c.saved.subscribe(() => (saved = true));

    c.submit();

    const req = http.expectOne('/api/v1/businesses/b1/agents');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual(
      expect.objectContaining({
        name: 'Triage Bot',
        provider: 'anthropic',
        model: 'claude-opus-4-8',
        allowed_tools: ['read_ticket'],
        monthly_budget_cents: 2500,
      }),
    );
    req.flush({
      id: 'a1', business_id: 'b1', principal_id: 'p1', name: 'Triage Bot', provider: 'anthropic',
      model: 'claude-opus-4-8', system_prompt: 'Be helpful', allowed_tools: ['read_ticket'],
      autonomy_mode: 1, enabled: true, monthly_budget_cents: 2500, allowed_mcp_servers: [],
      retriage_on_reply: false, created_at: '', updated_at: '',
    });
    expect(saved).toBe(true);
  });

  it('filters the model dropdown by provider', () => {
    const c = fixture.componentInstance;
    c.provider.set('openai');
    expect(c.modelsForProvider().map((m) => m.model_id)).toEqual(['gpt-5']);
    c.provider.set('anthropic');
    expect(c.modelsForProvider().map((m) => m.model_id)).toEqual(['claude-opus-4-8']);
  });

  it('shows an OpenRouter typeahead populated from the live provider catalog', () => {
    const c = fixture.componentInstance;
    c.onProviderChange('openrouter');
    http.expectOne('/api/v1/businesses/b1/agents/provider-models/openrouter').flush({
      items: [
        { provider: 'openrouter', model_id: 'openai/gpt-4o' },
        { provider: 'openrouter', model_id: 'anthropic/claude-3-haiku' },
      ],
    });
    fixture.detectChanges();

    expect(c.isFreeTextModel()).toBe(true);
    expect(c.isOpenRouter()).toBe(true);
    expect(c.openrouterModels().map((m) => m.model_id)).toEqual(['openai/gpt-4o', 'anthropic/claude-3-haiku']);

    // the input is a typeahead bound to the datalist of live models
    const el = fixture.nativeElement as HTMLElement;
    const input = el.querySelector('[data-testid="agent-model-text"]') as HTMLInputElement;
    expect(input.getAttribute('list')).toBe('openrouter-models');
    const opts = el.querySelectorAll('[data-testid="openrouter-model-options"] option');
    expect(opts.length).toBe(2);
    expect((opts[0] as HTMLOptionElement).value).toBe('openai/gpt-4o');

    // switching away clears the list and triggers no further fetch
    c.onProviderChange('anthropic');
    expect(c.isFreeTextModel()).toBe(false);
    expect(c.openrouterModels().length).toBe(0);
  });

  function flushAgent(req: TestRequest, lanes: number) {
    req.flush({
      id: 'a1', business_id: 'b1', principal_id: 'p1', name: 'GPU Bot', provider: 'vllm',
      model: 'ornith-1.0-9b', system_prompt: '', allowed_tools: [], autonomy_mode: 1, enabled: true,
      monthly_budget_cents: 0, allowed_mcp_servers: [], retriage_on_reply: false,
      max_concurrent_lanes: lanes, created_at: '', updated_at: '',
    });
  }

  it('sends the per-agent max_concurrent_lanes in the create payload', () => {
    const c = fixture.componentInstance;
    c.name = 'GPU Bot';
    c.provider.set('vllm');
    c.model = 'ornith-1.0-9b';
    c.maxConcurrentLanes = 1; // single-GPU self-host
    c.submit();
    const req = http.expectOne('/api/v1/businesses/b1/agents');
    expect(req.request.body).toEqual(expect.objectContaining({ max_concurrent_lanes: 1 }));
    flushAgent(req, 1);
  });

  it('clamps an out-of-range lanes value into [1,16] before sending', () => {
    const c = fixture.componentInstance;
    c.name = 'GPU Bot';
    c.provider.set('vllm');
    c.model = 'ornith-1.0-9b';
    c.maxConcurrentLanes = 99;
    c.submit();
    const req = http.expectOne('/api/v1/businesses/b1/agents');
    expect(req.request.body).toEqual(expect.objectContaining({ max_concurrent_lanes: 16 }));
    flushAgent(req, 16);
  });
});
