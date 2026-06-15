import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
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
});
