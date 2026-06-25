import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import {
  CodeReview,
  CodeReviewService,
  RepoConnector,
  TriggerReviewResponse,
} from './code-review.service';

describe('CodeReviewService', () => {
  let svc: CodeReviewService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    svc = TestBed.inject(CodeReviewService);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  // --- RepoConnector endpoints ---

  it('lists connectors', () => {
    let res: RepoConnector[] | undefined;
    svc.listConnectors('b1').subscribe((r) => (res = r.items));
    const req = http.expectOne('/api/v1/businesses/b1/repo-connectors');
    expect(req.request.method).toBe('GET');
    req.flush({
      items: [
        {
          id: 'c1',
          type: 'github',
          display_name: 'My Connector',
          base_url: 'https://github.com',
          repo: 'org/repo',
          allow_private_base_url: false,
          status: 'active',
          created_at: '2026-01-01T00:00:00Z',
        },
      ],
    });
    expect(res?.[0].repo).toBe('org/repo');
    expect(res?.[0].id).toBe('c1');
  });

  it('creates a connector', () => {
    let res: { id: string } | undefined;
    svc
      .createConnector('b1', {
        type: 'github',
        display_name: 'My Connector',
        base_url: '',
        repo: 'org/repo',
        api_token: 'tok_abc',
        allow_private_base_url: false,
      })
      .subscribe((r) => (res = r));
    const req = http.expectOne('/api/v1/businesses/b1/repo-connectors');
    expect(req.request.method).toBe('POST');
    expect(req.request.body.repo).toBe('org/repo');
    expect(req.request.body.api_token).toBe('tok_abc');
    req.flush({ id: 'c2' });
    expect(res?.id).toBe('c2');
  });

  it('deletes a connector', () => {
    let called = false;
    svc.deleteConnector('b1', 'c1').subscribe(() => (called = true));
    const req = http.expectOne('/api/v1/businesses/b1/repo-connectors/c1');
    expect(req.request.method).toBe('DELETE');
    req.flush(null, { status: 204, statusText: 'No Content' });
    expect(called).toBe(true);
  });

  // --- CodeReview endpoints ---

  it('lists reviews', () => {
    let res: CodeReview[] | undefined;
    svc.listReviews('b1').subscribe((r) => (res = r.items));
    const req = http.expectOne('/api/v1/businesses/b1/code-reviews');
    expect(req.request.method).toBe('GET');
    req.flush({
      items: [
        {
          id: 'r1',
          status: 'succeeded',
          summary: 'Looks good',
          review_url: '',
          pr_number: 42,
          findings: [],
          findings_count: 0,
          created_at: '2026-01-01T00:00:00Z',
          posted_at: null,
        },
      ],
    });
    expect(res?.[0].pr_number).toBe(42);
    expect(res?.[0].status).toBe('succeeded');
  });

  it('gets a single review', () => {
    let res: CodeReview | undefined;
    svc.getReview('b1', 'r1').subscribe((r) => (res = r));
    const req = http.expectOne('/api/v1/businesses/b1/code-reviews/r1');
    expect(req.request.method).toBe('GET');
    req.flush({
      id: 'r1',
      status: 'succeeded',
      summary: 'All clear',
      review_url: 'https://github.com/org/repo/pull/42#issuecomment-1',
      pr_number: 42,
      findings: [
        {
          file: 'src/main.go',
          line: 10,
          severity: 'warning',
          title: 'Unused variable',
          detail: 'x is declared but not used',
        },
      ],
      findings_count: 1,
      created_at: '2026-01-01T00:00:00Z',
      posted_at: '2026-01-01T01:00:00Z',
    });
    expect(res?.id).toBe('r1');
    expect(res?.findings?.[0].file).toBe('src/main.go');
    expect(res?.findings?.[0].severity).toBe('warning');
    expect(res?.review_url).toBe('https://github.com/org/repo/pull/42#issuecomment-1');
  });

  it('triggers a review (202 pending)', () => {
    let res: TriggerReviewResponse | undefined;
    svc
      .trigger('b1', { agent_id: 'a1', repo_connector_id: 'c1', pr_number: 5 })
      .subscribe((r) => (res = r));
    const req = http.expectOne('/api/v1/businesses/b1/code-reviews');
    expect(req.request.method).toBe('POST');
    expect(req.request.body.pr_number).toBe(5);
    expect(req.request.body.agent_id).toBe('a1');
    expect(req.request.body.repo_connector_id).toBe('c1');
    req.flush({ id: 'r1', status: 'pending', review_url: '' });
    expect(res?.id).toBe('r1');
    expect(res?.status).toBe('pending');
  });
});
