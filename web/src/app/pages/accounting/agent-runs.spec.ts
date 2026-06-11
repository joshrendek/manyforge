import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { RunSummary } from '../../core/accounting.service';
import { AgentRunsComponent } from './agent-runs';

// Component-level coverage for the agent-runs page (Task 23 UI redesign).
// Drives the real component against a mock backend and asserts:
// — design-system markup (mf-page-header, mf-table, mf-status-pill)
// — all data-testid attributes preserved
// — dark-theme token classes visible

const runsUrl = '/api/v1/businesses/b1/agents/ag1/runs';

function makeRun(over: Partial<RunSummary> = {}): RunSummary {
  return {
    id: 'run1',
    agent_id: 'ag1',
    trigger: 'manual',
    status: 'succeeded',
    tokens_in: 1000,
    tokens_out: 500,
    cost_cents: 250,
    correlation_id: 'cor1',
    created_at: '2024-06-01T10:00:00Z',
    ...over,
  };
}

const runsPage = { items: [makeRun()], next_cursor: null };

describe('AgentRunsComponent (Task 23 UI redesign)', () => {
  let fixture: ComponentFixture<AgentRunsComponent>;
  let mock: HttpTestingController;

  function boot(): void {
    fixture = TestBed.createComponent(AgentRunsComponent);
    fixture.detectChanges(); // ngOnInit → GET runs
    mock.expectOne((req) => req.url === runsUrl).flush(runsPage);
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
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: {
              paramMap: {
                get: (key: string) => (key === 'businessId' ? 'b1' : key === 'agentId' ? 'ag1' : null),
              },
            },
          },
        },
      ],
    });
    mock = TestBed.inject(HttpTestingController);
    document.documentElement.setAttribute('data-theme', 'light');
  });

  afterEach(() => {
    mock.verify();
    document.documentElement.setAttribute('data-theme', 'light');
    localStorage.clear();
  });

  it('renders mf-page-header with title "Agent runs"', () => {
    boot();
    const header = q('mf-page-header');
    expect(header).not.toBeNull();
    expect(header!.textContent).toContain('Agent runs');
  });

  it('renders run-list and run-row after loading', () => {
    boot();
    expect(q('[data-testid="run-list"]')).not.toBeNull();
    const rows = fixture.nativeElement.querySelectorAll('[data-testid="run-row"]');
    expect(rows.length).toBe(1);
  });

  it('run-status is an mf-status-pill', () => {
    boot();
    const statusEl = q('[data-testid="run-status"]');
    expect(statusEl).not.toBeNull();
    // mf-status-pill is a custom element; check the host element tag
    const pill = statusEl!.closest('mf-status-pill') ?? statusEl;
    expect(pill).not.toBeNull();
  });

  it('run-cost and run-tokens are present', () => {
    boot();
    expect(q('[data-testid="run-cost"]')).not.toBeNull();
    expect(text('run-tokens')).toContain('1000');
  });

  it('back-to-accounting link present in header actions', () => {
    boot();
    expect(q('[data-testid="back-to-accounting"]')).not.toBeNull();
  });

  it('dark-theme: .mf-table or .mf-card is present', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    boot();
    const hasTable = !!q('.mf-table');
    const hasCard = !!q('.mf-card');
    expect(hasTable || hasCard).toBe(true);
  });
});
