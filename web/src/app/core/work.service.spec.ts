import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { WorkService } from './work.service';

const base = '/api/v1/businesses/b1/work';

describe('WorkService', () => {
  let mock: HttpTestingController;
  let svc: WorkService;
  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    mock = TestBed.inject(HttpTestingController);
    svc = TestBed.inject(WorkService);
  });
  afterEach(() => mock.verify());

  it('summary GETs the business summary and sets openOperatorCount', () => {
    svc.summary('b1').subscribe();
    mock.expectOne(`${base}/summary`).flush({ open_operator: 3, open_agent: 5, in_progress: 1 });
    expect(svc.openOperatorCount()).toBe(3);
  });

  it('refreshSummary zeroes the count on failure', () => {
    svc.openOperatorCount.set(4);
    svc.refreshSummary('b1');
    mock.expectOne(`${base}/summary`).flush('nope', { status: 500, statusText: 'err' });
    expect(svc.openOperatorCount()).toBe(0);
  });

  it('lists and creates projects', () => {
    svc.listProjects('b1').subscribe();
    expect(mock.expectOne(`${base}/projects`).request.method).toBe('GET');
    svc.createProject('b1', { name: 'ManyForge', key_prefix: 'mf' }).subscribe();
    const req = mock.expectOne(`${base}/projects`);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ name: 'ManyForge', key_prefix: 'mf' });
  });

  it('lists tasks, optionally including closed ones', () => {
    svc.listTasks('b1', 'p1').subscribe();
    mock.expectOne(`${base}/projects/p1/tasks`).flush({ items: [] });
    svc.listTasks('b1', 'p1', { includeClosed: true }).subscribe();
    mock.expectOne(`${base}/projects/p1/tasks?include_closed=true`).flush({ items: [] });
  });

  it('creates a task with POST and patches one with PATCH by key', () => {
    svc.createTask('b1', 'p1', { title: 'Rotate key', lane: 'operator' }).subscribe();
    const create = mock.expectOne(`${base}/projects/p1/tasks`);
    expect(create.request.method).toBe('POST');
    expect(create.request.body).toEqual({ title: 'Rotate key', lane: 'operator' });

    svc.updateTask('b1', 'p1', 'mf-abc123', { status: 'done', note: 'did it' }).subscribe();
    const patch = mock.expectOne(`${base}/projects/p1/tasks/mf-abc123`);
    expect(patch.request.method).toBe('PATCH');
    expect(patch.request.body).toEqual({ status: 'done', note: 'did it' });
  });

  it('manages tokens: list, create with a name, revoke with DELETE', () => {
    svc.listTokens('b1', 'p1').subscribe();
    mock.expectOne(`${base}/projects/p1/tokens`).flush({ items: [] });
    svc.createToken('b1', 'p1', 'laptop').subscribe();
    const create = mock.expectOne(`${base}/projects/p1/tokens`);
    expect(create.request.method).toBe('POST');
    expect(create.request.body).toEqual({ name: 'laptop' });
    svc.revokeToken('b1', 'p1', 't1').subscribe();
    expect(mock.expectOne(`${base}/projects/p1/tokens/t1`).request.method).toBe('DELETE');
  });

  it('refuses path segments that could traverse to another API route (404, no request)', () => {
    const cases: [string, () => import('rxjs').Observable<unknown>][] = [
      ['business ..', () => svc.listProjects('..')],
      ['business with slash', () => svc.listProjects('b1/../../account')],
      ['project ..', () => svc.listTasks('b1', '..')],
      ['project encoded', () => svc.getProject('b1', '%2e%2e')],
      ['task key ..', () => svc.updateTask('b1', 'p1', '..', { status: 'done' })],
      ['token id ..', () => svc.revokeToken('b1', 'p1', '../../tokens')],
    ];
    for (const [name, call] of cases) {
      let status = 0;
      call().subscribe({ error: (e) => (status = e.status) });
      expect(status, name).toBe(404);
    }
    mock.verify();
  });
});
