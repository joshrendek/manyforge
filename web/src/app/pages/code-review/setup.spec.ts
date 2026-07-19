import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ReviewConfig, ReviewDimension, ReviewDimensionFallbackEntry } from '../../core/code-review.service';
import { CdkDragDrop } from '@angular/cdk/drag-drop';
import { CodeReviewSetupComponent } from './setup';

const businesses = {
  items: [
    { id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true },
    { id: 'b2', parent_id: null, tenant_root_id: 'b2', name: 'Beta', status: 'active', is_tenant_root: true },
  ],
  next_cursor: null,
};

const models = { items: [{ provider: 'anthropic', model_id: 'claude-opus-4-8' }] };

const agentsList = {
  items: [
    { id: 'ag1', name: 'LM Studio' },
    { id: 'ag2', name: 'Cloud' },
  ],
};

function makeDim(over: Partial<ReviewDimension> = {}): ReviewDimension {
  return {
    id: 'd1',
    dimension: 'security',
    provider: '',
    model: '',
    fallback_chain: [],
    prompt: 'Security prompt',
    scope_globs: [],
    min_severity: 'warning',
    enabled: true,
    sort_order: 1,
    ...over,
  };
}

const defaultConfig: ReviewConfig = {
  dedupe: true,
  verify_enabled: false,
  verify_provider: '',
  verify_model: '',
  cite_rules: false,
  post_mode: 'single',
  review_agent_chain: [],
};

describe('CodeReviewSetupComponent', () => {
  let fixture: ComponentFixture<CodeReviewSetupComponent>;
  let cmp: CodeReviewSetupComponent;
  let mock: HttpTestingController;

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }
  function qAll(sel: string): NodeListOf<HTMLElement> {
    return fixture.nativeElement.querySelectorAll(sel) as NodeListOf<HTMLElement>;
  }

  beforeEach(() => {
    localStorage.clear();
    localStorage.setItem('mf-current-business', 'b1');
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    mock = TestBed.inject(HttpTestingController);
  });

  afterEach(() => {
    mock.verify();
    localStorage.clear();
  });

  /** Mount and flush the initial loads (businesses → panel: dimensions, config, models). */
  function mount(dims: ReviewDimension[] = [makeDim()], config: ReviewConfig = defaultConfig): void {
    fixture = TestBed.createComponent(CodeReviewSetupComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(businesses);
    fixture.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/review-dimensions').flush({ items: dims });
    mock.expectOne('/api/v1/businesses/b1/review-config').flush(config);
    mock.expectOne('/api/v1/businesses/b1/agents/models').flush(models);
    mock.expectOne('/api/v1/businesses/b1/agents').flush(agentsList);
    fixture.detectChanges();
  }

  it('renders a business selector seeded from the current business', () => {
    mount();
    const sel = q('[data-testid="setup-business"]') as HTMLSelectElement;
    expect(sel).toBeTruthy();
    expect(cmp.businessId()).toBe('b1');
    const opts = Array.from(qAll('[data-testid="setup-business"] option')).map((o) => (o as HTMLOptionElement).value);
    expect(opts).toEqual(['b1', 'b2']);
  });

  it('loads and renders configured dimension rows', () => {
    mount([makeDim({ dimension: 'security' }), makeDim({ id: 'd2', dimension: 'correctness', min_severity: 'info' })]);
    expect(qAll('[data-testid="dimension-row"]').length).toBe(2);
    expect(fixture.nativeElement.textContent).toContain('Security');
    expect(fixture.nativeElement.textContent).toContain('Correctness');
  });

  it('shows an empty state when no dimensions are configured', () => {
    mount([]);
    expect(q('[data-testid="dimensions-empty"]')).toBeTruthy();
    expect(qAll('[data-testid="dimension-row"]').length).toBe(0);
  });

  it('applies the Balanced preset, seeding four editable rows from the catalog', () => {
    mount([]);
    (q('[data-testid="preset-balanced"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    const rows = qAll('[data-testid="dimension-row"]');
    expect(rows.length).toBe(4);
    const text = fixture.nativeElement.textContent as string;
    expect(text).toContain('Security');
    expect(text).toContain('Correctness');
    expect(text).toContain('Performance');
    expect(text).toContain('Tests');
  });

  it('saves a row via POST to review-dimensions with the built input body', () => {
    mount([makeDim({ dimension: 'security', model: 'x' })]);
    (q('[data-testid="row-save"]') as HTMLButtonElement).click();
    const req = mock.expectOne('/api/v1/businesses/b1/review-dimensions');
    expect(req.request.method).toBe('POST');
    expect(req.request.body.dimension).toBe('security');
    expect(req.request.body.min_severity).toBe('warning');
    expect(Array.isArray(req.request.body.scope_globs)).toBe(true);
    req.flush(makeDim({ dimension: 'security', model: 'x' }));
  });

  it('adds, reorders, and removes providers in the unified priority list via the row controls', () => {
    mount([makeDim({ dimension: 'security', model: 'x' })]);
    // A dimension always starts with exactly one entry — the primary (#1).
    expect(cmp.rows()[0].chain.length).toBe(1);
    expect(qAll('[data-testid="row-priority-entry-1"]').length).toBe(0);

    // Add appends a blank entry.
    (q('[data-testid="row-priority-add"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(cmp.rows()[0].chain.length).toBe(2);

    // Choosing a provider for entry 0 (the primary) resets its model (onPriorityProviderChange).
    const p0 = q('[data-testid="row-priority-provider-0"]') as HTMLSelectElement;
    p0.value = 'ollama';
    p0.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    expect(cmp.rows()[0].chain[0]).toEqual({ provider: 'ollama', model: '' });
    const m0 = q('[data-testid="row-priority-model-text-0"]') as HTMLInputElement;
    m0.value = 'llama3';
    m0.dispatchEvent(new Event('input'));
    fixture.detectChanges();

    const p1 = q('[data-testid="row-priority-provider-1"]') as HTMLSelectElement;
    p1.value = 'openrouter';
    p1.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    // Selecting openrouter lazily fetches its live model catalog for the typeahead datalist.
    mock.expectOne('/api/v1/businesses/b1/agents/provider-models/openrouter').flush({ items: [] });
    const m1 = q('[data-testid="row-priority-model-text-1"]') as HTMLInputElement;
    m1.value = 'deepseek';
    m1.dispatchEvent(new Event('input'));
    fixture.detectChanges();
    expect(cmp.rows()[0].chain).toEqual([
      { provider: 'ollama', model: 'llama3' },
      { provider: 'openrouter', model: 'deepseek' },
    ]);

    // Reorder: move entry 1 up → openrouter becomes the primary (#1).
    (q('[data-testid="row-priority-up-1"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(cmp.rows()[0].chain).toEqual([
      { provider: 'openrouter', model: 'deepseek' },
      { provider: 'ollama', model: 'llama3' },
    ]);

    // Remove entry 1 → one primary remains, and its Remove is disabled (a dimension keeps a #1).
    (q('[data-testid="row-priority-remove-1"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(cmp.rows()[0].chain).toEqual([{ provider: 'openrouter', model: 'deepseek' }]);
    expect(qAll('[data-testid="row-priority-entry-1"]').length).toBe(0);
    expect((q('[data-testid="row-priority-remove-0"]') as HTMLButtonElement).disabled).toBe(true);
  });

  it('maps the priority list to provider/model + fallback_chain, dropping blank fallbacks', () => {
    mount([makeDim({ dimension: 'security', model: 'x' })]);
    cmp.rows()[0].chain = [
      { provider: 'openrouter', model: 'deepseek' }, // primary → provider/model
      { provider: 'ollama', model: 'llama3' }, // fallback → fallback_chain[0]
      { provider: '', model: '' }, // blank fallback → dropped
    ];
    (q('[data-testid="row-save"]') as HTMLButtonElement).click();
    const req = mock.expectOne('/api/v1/businesses/b1/review-dimensions');
    expect(req.request.body.provider).toBe('openrouter');
    expect(req.request.body.model).toBe('deepseek');
    expect(req.request.body.fallback_chain).toEqual([{ provider: 'ollama', model: 'llama3' }]);
    req.flush(makeDim({ dimension: 'security', model: 'x' }));
  });

  it('deletes a persisted row via DELETE', () => {
    mount([makeDim({ id: 'd9', dimension: 'security' })]);
    (q('[data-testid="row-remove"]') as HTMLButtonElement).click();
    const req = mock.expectOne('/api/v1/businesses/b1/review-dimensions/d9');
    expect(req.request.method).toBe('DELETE');
    req.flush(null);
    fixture.detectChanges();
    expect(qAll('[data-testid="dimension-row"]').length).toBe(0);
  });

  it('drops an unsaved (preset-seeded) row locally without a DELETE call', () => {
    mount([]);
    (q('[data-testid="preset-fast"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(qAll('[data-testid="dimension-row"]').length).toBe(2);
    (q('[data-testid="row-remove"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    // no DELETE fired (unsaved row) — mock.verify() in afterEach asserts that
    expect(qAll('[data-testid="dimension-row"]').length).toBe(1);
  });

  it('saves panel config via PUT to review-config', () => {
    mount([makeDim()], { ...defaultConfig, dedupe: true });
    const dedupe = q('[data-testid="config-dedupe"]') as HTMLInputElement;
    dedupe.checked = false; // toggle off
    dedupe.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    (q('[data-testid="config-save"]') as HTMLButtonElement).click();
    const req = mock.expectOne('/api/v1/businesses/b1/review-config');
    expect(req.request.method).toBe('PUT');
    expect(req.request.body.dedupe).toBe(false);
    expect(req.request.body.post_mode).toBe('single');
    req.flush({ ...defaultConfig, dedupe: false });
  });

  it('edits the reviewbot fallback chain (add, reorder, remove) and saves it in order', () => {
    mount();
    const add = () => q('[data-testid="chain-add"]') as HTMLSelectElement;
    // add LM Studio (ag1) then Cloud (ag2)
    add().value = 'ag1';
    add().dispatchEvent(new Event('change'));
    fixture.detectChanges();
    add().value = 'ag2';
    add().dispatchEvent(new Event('change'));
    fixture.detectChanges();
    expect(cmp.config().review_agent_chain).toEqual(['ag1', 'ag2']);
    expect((q('[data-testid="chain-name-0"]') as HTMLElement).textContent).toContain('LM Studio');

    // reorder: move the 2nd entry up → [ag2, ag1]
    (q('[data-testid="chain-up-1"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(cmp.config().review_agent_chain).toEqual(['ag2', 'ag1']);

    // remove the 1st entry → [ag1]
    (q('[data-testid="chain-remove-0"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(cmp.config().review_agent_chain).toEqual(['ag1']);

    // save → the PUT body carries the chain in order
    (q('[data-testid="config-save"]') as HTMLButtonElement).click();
    const req = mock.expectOne('/api/v1/businesses/b1/review-config');
    expect(req.request.method).toBe('PUT');
    expect(req.request.body.review_agent_chain).toEqual(['ag1']);
    req.flush({ ...defaultConfig, review_agent_chain: ['ag1'] });
  });

  it('reloads the panel when a different business is selected', () => {
    mount();
    const sel = q('[data-testid="setup-business"]') as HTMLSelectElement;
    sel.value = 'b2';
    sel.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    // Selecting b2 refetches its panel.
    mock.expectOne('/api/v1/businesses/b2/review-dimensions').flush({ items: [] });
    mock.expectOne('/api/v1/businesses/b2/review-config').flush(defaultConfig);
    mock.expectOne('/api/v1/businesses/b2/agents/models').flush(models);
    mock.expectOne('/api/v1/businesses/b2/agents').flush({ items: [] });
    fixture.detectChanges();
    expect(cmp.businessId()).toBe('b2');
  });

  it('promotes a fallback to primary via drag-drop (onPriorityDrop)', () => {
    mount([makeDim({ dimension: 'security', model: 'x' })]);
    const row = cmp.rows()[0];
    row.chain = [
      { provider: 'ollama', model: 'llama3' }, // primary (#1)
      { provider: 'openrouter', model: 'gpt-4o' }, // fallback
    ];
    // Drag the fallback (index 1) to the top → it becomes the primary.
    cmp.onPriorityDrop(row, { previousIndex: 1, currentIndex: 0 } as CdkDragDrop<ReviewDimensionFallbackEntry[]>);
    expect(cmp.rows()[0].chain.map((e) => e.provider)).toEqual(['openrouter', 'ollama']);
  });

  it('reorders the reviewbot chain via drag-drop (onChainDrop)', () => {
    mount([makeDim()], { ...defaultConfig, review_agent_chain: ['ag1', 'ag2'] });
    cmp.onChainDrop({ previousIndex: 0, currentIndex: 1 } as CdkDragDrop<string[]>);
    expect(cmp.config().review_agent_chain).toEqual(['ag2', 'ag1']);
  });
});
