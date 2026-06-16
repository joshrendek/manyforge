import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { AgentsService } from './agents.service';

describe('AgentsService.run', () => {
  let svc: AgentsService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    svc = TestBed.inject(AgentsService);
    http = TestBed.inject(HttpTestingController);
  });

  it('POSTs the runs endpoint with the ticket target and returns the run', () => {
    let got: { id: string; status: string } | undefined;
    svc.run('b1', 'a1', { target_type: 'ticket', target_id: 't1' }).subscribe((r) => (got = r));
    const req = http.expectOne('/api/v1/businesses/b1/agents/a1/runs');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ target_type: 'ticket', target_id: 't1' });
    req.flush({ id: 'run1', agent_id: 'a1', trigger: 'manual', status: 'awaiting_approval', tokens_in: 0, tokens_out: 0, cost_cents: 0, correlation_id: 'c1' });
    expect(got!.status).toBe('awaiting_approval');
  });
});
