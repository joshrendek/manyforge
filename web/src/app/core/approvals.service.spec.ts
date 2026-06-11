import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ApprovalsService } from './approvals.service';

describe('ApprovalsService', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => mock.verify());

  it('lists pending and updates pendingCount', () => {
    const svc = TestBed.inject(ApprovalsService);
    let got: any;
    svc.listPending('b1').subscribe((r) => (got = r));
    mock.expectOne('/api/v1/businesses/b1/approvals').flush({
      items: [{ id: 'a1', agent_run_id: 'r1', tool: 'add_external_comment', effect_class: 2, state: 'pending', expires_at: '2026-07-01T00:00:00Z', summary: 'Comment on ticket abc' }],
    });
    expect(got.items.length).toBe(1);
  });

  it('approve POSTs to the approve path', () => {
    const svc = TestBed.inject(ApprovalsService);
    svc.approve('b1', 'a1').subscribe();
    const req = mock.expectOne('/api/v1/businesses/b1/approvals/a1/approve');
    expect(req.request.method).toBe('POST');
    req.flush({ id: 'a1', state: 'approved' });
  });

  it('refreshCount sets pendingCount from items length', () => {
    const svc = TestBed.inject(ApprovalsService);
    svc.refreshCount('b1');
    mock.expectOne('/api/v1/businesses/b1/approvals').flush({ items: [{ id: 'a1' }, { id: 'a2' }] });
    expect(svc.pendingCount()).toBe(2);
  });
});
