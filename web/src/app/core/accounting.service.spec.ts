import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AccountingService } from './accounting.service';

describe('AccountingService', () => {
  let svc: AccountingService;
  let mock: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    svc = TestBed.inject(AccountingService);
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => mock.verify());

  it('getSummary hits the accounting endpoint with the window param', () => {
    svc.getSummary('biz-1', 'last_month').subscribe();
    const req = mock.expectOne((r) => r.url === '/api/v1/businesses/biz-1/accounting');
    expect(req.request.params.get('window')).toBe('last_month');
    req.flush({
      window: { from: '', to: '' },
      totals: { cost_cents: 0, tokens_in: 0, tokens_out: 0, run_count: 0 },
      agents: [],
    });
  });

  it('listRuns hits the runs endpoint with status + cursor', () => {
    svc.listRuns('biz-1', 'ag-1', { status: 'succeeded', cursor: 'abc' }).subscribe();
    const req = mock.expectOne((r) => r.url === '/api/v1/businesses/biz-1/agents/ag-1/runs');
    expect(req.request.params.get('status')).toBe('succeeded');
    expect(req.request.params.get('cursor')).toBe('abc');
    req.flush({ items: [], next_cursor: null });
  });
});
