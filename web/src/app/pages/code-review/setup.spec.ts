import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ReviewConfig, ReviewDimension } from '../../core/code-review.service';
import { CodeReviewSetupComponent } from './setup';

const businesses = {
  items: [
    { id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true },
    { id: 'b2', parent_id: null, tenant_root_id: 'b2', name: 'Beta', status: 'active', is_tenant_root: true },
  ],
  next_cursor: null,
};

const models = { items: [{ provider: 'anthropic', model_id: 'claude-opus-4-8' }] };

function makeDim(over: Partial<ReviewDimension> = {}): ReviewDimension {
  return {
    id: 'd1',
    dimension: 'security',
    provider: '',
    model: '',
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
    fixture.detectChanges();
    expect(cmp.businessId()).toBe('b2');
  });
});
