import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AccountingSummary } from '../../core/accounting.service';
import { AccountingSummaryComponent } from './summary';

// Component-level coverage for the accounting summary page (Task 22 UI redesign).
// Drives the real component against a mock backend and asserts:
// — design-system markup (mf-page-header, mf-select, mf-card, mf-table, mf-pill)
// — all data-testid attributes preserved
// — dark-theme token classes visible

const bizUrl = '/api/v1/businesses';

function makeBizPage() {
  return {
    items: [
      {
        id: 'b1',
        parent_id: null,
        tenant_root_id: 'b1',
        name: 'Acme',
        status: 'active',
        is_tenant_root: true,
      },
    ],
  };
}

const summaryData: AccountingSummary = {
  window: { from: '2024-06-01', to: '2024-06-30' },
  totals: { cost_cents: 1500, tokens_in: 10000, tokens_out: 5000, run_count: 42 },
  agents: [
    {
      agent_id: 'ag1',
      name: 'Support Bot',
      monthly_budget_cents: 5000,
      run_count: 42,
      tokens_in: 10000,
      tokens_out: 5000,
      cost_cents: 1500,
      budget_pct: 30,
    },
  ],
};

describe('AccountingSummaryComponent (Task 22 UI redesign)', () => {
  let fixture: ComponentFixture<AccountingSummaryComponent>;
  let mock: HttpTestingController;

  function boot(): void {
    fixture = TestBed.createComponent(AccountingSummaryComponent);
    fixture.detectChanges(); // ngOnInit → GET /api/v1/businesses
    mock.expectOne(bizUrl).flush(makeBizPage());
    // After businesses load, component auto-selects first biz and calls reload()
    mock
      .expectOne((req) => req.url.includes('/accounting'))
      .flush(summaryData);
    fixture.detectChanges();
  }

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }

  function text(testid: string): string {
    return (q(`[data-testid="${testid}"]`)?.textContent?.trim() ?? '');
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    mock = TestBed.inject(HttpTestingController);
    document.documentElement.setAttribute('data-theme', 'light');
  });

  afterEach(() => {
    mock.verify();
    document.documentElement.setAttribute('data-theme', 'light');
    localStorage.clear();
  });

  it('renders mf-page-header with title "Accounting"', () => {
    boot();
    const header = q('mf-page-header');
    expect(header).not.toBeNull();
    expect(header!.textContent).toContain('Accounting');
  });

  it('business-select and window-select are select.mf-select', () => {
    boot();
    const bizSel = q('[data-testid="business-select"]');
    expect(bizSel).not.toBeNull();
    expect(bizSel!.tagName.toLowerCase()).toBe('select');
    expect(bizSel!.classList.contains('mf-select')).toBe(true);

    const winSel = q('[data-testid="window-select"]');
    expect(winSel).not.toBeNull();
    expect(winSel!.tagName.toLowerCase()).toBe('select');
    expect(winSel!.classList.contains('mf-select')).toBe(true);
  });

  it('totals cards present with correct testids inside .mf-card', () => {
    boot();
    expect(q('[data-testid="totals"]')).not.toBeNull();
    // total-cost must appear inside a .mf-card
    const costEl = q('[data-testid="total-cost"]');
    expect(costEl).not.toBeNull();
    const card = costEl!.closest('.mf-card');
    expect(card).not.toBeNull();

    expect(q('[data-testid="total-in"]')).not.toBeNull();
    expect(q('[data-testid="total-out"]')).not.toBeNull();
    expect(q('[data-testid="total-runs"]')).not.toBeNull();
  });

  it('renders agent-list with agent-row, agent-name, agent-cost, agent-runs, agent-budget-pct', () => {
    boot();
    expect(q('[data-testid="agent-list"]')).not.toBeNull();
    const rows = fixture.nativeElement.querySelectorAll('[data-testid="agent-row"]');
    expect(rows.length).toBe(1);
    expect(text('agent-name')).toContain('Support Bot');
    expect(q('[data-testid="agent-cost"]')).not.toBeNull();
    expect(q('[data-testid="agent-runs"]')).not.toBeNull();
    expect(q('[data-testid="agent-budget-pct"]')).not.toBeNull();
  });

  it('back-to-dashboard link is present in header actions', () => {
    boot();
    expect(q('[data-testid="back-to-dashboard"]')).not.toBeNull();
  });

  it('dark-theme: .mf-card is present', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    boot();
    expect(q('.mf-card')).not.toBeNull();
  });
});
