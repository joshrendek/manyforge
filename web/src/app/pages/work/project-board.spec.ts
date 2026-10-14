import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, Router, convertToParamMap, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { WorkTask } from '../../core/work.service';
import { ToastService } from '../../ui/toast/toast.service';
import { WorkProjectBoardComponent } from './project-board';
import { makeProject, makeTask } from './work-fixtures';

const base = '/api/v1/businesses/b1/work';

const TASKS: WorkTask[] = [
  makeTask({ key: 'mf-todo01', title: 'Wire sync', status: 'todo', priority: 1 }),
  makeTask({ key: 'mf-prog01', title: 'Write CLI', status: 'in_progress', assignee: 'claude' }),
  makeTask({
    key: 'mf-blck01',
    title: 'Ship it',
    status: 'blocked',
    blocked_by: ['mf-oper01'],
  }),
  makeTask({
    key: 'mf-oper01',
    title: 'Rotate the Stripe key',
    description: 'Dashboard → Developers → API keys.\nRoll the live key.',
    lane: 'operator',
    status: 'todo',
    priority: 0,
  }),
  makeTask({ key: 'mf-oper02', title: 'Approve DNS', lane: 'operator', status: 'backlog' }),
  makeTask({ key: 'mf-back01', title: 'Someday', status: 'backlog', priority: 3 }),
  makeTask({
    key: 'mf-done01',
    title: 'Old work',
    status: 'done',
    closed_at: '2026-10-12T00:00:00Z',
  }),
];

describe('WorkProjectBoardComponent', () => {
  let fixture: ComponentFixture<WorkProjectBoardComponent>;
  let cmp: WorkProjectBoardComponent;
  let mock: HttpTestingController;
  let query: Record<string, string>;

  function load(tasks = TASKS): void {
    fixture = TestBed.createComponent(WorkProjectBoardComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges();
    mock.expectOne(`${base}/projects/p1`).flush(makeProject());
    mock.expectOne(`${base}/projects/p1/tasks`).flush({ items: tasks });
    fixture.detectChanges();
  }

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }
  function qa(sel: string): HTMLElement[] {
    return Array.from(fixture.nativeElement.querySelectorAll(sel)) as HTMLElement[];
  }
  function keysIn(sel: string): string[] {
    return qa(`${sel} [data-key]`).map((e) => e.getAttribute('data-key') ?? '');
  }

  beforeEach(() => {
    query = {};
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useFactory: () => ({
            snapshot: {
              paramMap: convertToParamMap({ businessId: 'b1', pid: 'p1' }),
              queryParamMap: convertToParamMap(query),
            },
          }),
        },
      ],
    });
    mock = TestBed.inject(HttpTestingController);
    vi.spyOn(TestBed.inject(Router), 'navigate').mockResolvedValue(true);
  });
  afterEach(() => {
    mock.verify();
    vi.useRealTimers();
  });

  it('places active tasks in their board columns, highest priority first', () => {
    load();
    expect(q('[data-testid="work-project-page"]')?.textContent).toContain('ManyForge');
    expect(keysIn('[data-testid="work-col-todo"]')).toEqual(['mf-oper01', 'mf-todo01']);
    expect(keysIn('[data-testid="work-col-in_progress"]')).toEqual(['mf-prog01']);
    expect(keysIn('[data-testid="work-col-blocked"]')).toEqual(['mf-blck01']);
    expect(keysIn('[data-testid="work-col-done"]')).toEqual(['mf-done01']);
    // Backlog tasks are not on the active board.
    expect(keysIn('[data-testid="work-board"]')).not.toContain('mf-back01');
    expect(q('[data-testid="work-card-blocked-by"]')?.textContent).toContain('mf-oper01');
    expect(q('[data-key="mf-oper01"] [data-testid="work-card-operator"]')).toBeTruthy();
  });

  it('filters the board by lane', () => {
    load();
    (q('[data-testid="work-lane-operator"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(keysIn('[data-testid="work-board"]')).toEqual(['mf-oper01']);
    (q('[data-testid="work-lane-agent"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(keysIn('[data-testid="work-board"]')).not.toContain('mf-oper01');
    expect(q('[data-testid="work-lane-agent"]')?.getAttribute('aria-pressed')).toBe('true');
  });

  it('shows the open operator count on the Operator tab', () => {
    load();
    expect(q('[data-testid="work-operator-count"]')?.textContent?.trim()).toBe('2');
  });

  it('operator inbox lists only open operator tasks with their instructions', () => {
    load();
    cmp.setTab('operator');
    fixture.detectChanges();
    expect(keysIn('[data-testid="work-operator-list"]')).toEqual(['mf-oper01', 'mf-oper02']);
    expect(q('[data-testid="work-operator-description"]')?.textContent).toBe(
      'Dashboard → Developers → API keys.\nRoll the live key.',
    );
  });

  it('Mark done PATCHes status done, then refreshes the list and the nav summary', () => {
    load();
    cmp.setTab('operator');
    fixture.detectChanges();
    (q('[data-key="mf-oper01"] [data-testid="work-operator-done"]') as HTMLButtonElement).click();
    const req = mock.expectOne(`${base}/projects/p1/tasks/mf-oper01`);
    expect(req.request.method).toBe('PATCH');
    expect(req.request.body).toEqual({ status: 'done' });
    req.flush({ ...TASKS[3], status: 'done', closed_at: '2026-10-14T00:00:00Z' });
    mock.expectOne(`${base}/summary`).flush({ open_operator: 1, open_agent: 0, in_progress: 0 });
    mock.expectOne(`${base}/projects/p1/tasks`).flush({ items: TASKS });
    expect(TestBed.inject(ToastService).toasts().length).toBeGreaterThan(0);
  });

  it('moves a card with its status select', () => {
    load();
    cmp.update(TASKS[0], { status: 'in_progress' });
    const req = mock.expectOne(`${base}/projects/p1/tasks/mf-todo01`);
    expect(req.request.body).toEqual({ status: 'in_progress' });
    req.flush({ ...TASKS[0], status: 'in_progress' });
    mock.expectOne(`${base}/summary`).flush({ open_operator: 2, open_agent: 0, in_progress: 2 });
    fixture.detectChanges();
    expect(keysIn('[data-testid="work-col-in_progress"]')).toContain('mf-todo01');
    mock.expectOne(`${base}/projects/p1/tasks`).flush({ items: TASKS });
  });

  it('a failed move toasts and re-reads the server state', () => {
    load();
    cmp.update(TASKS[0], { status: 'done' });
    mock
      .expectOne(`${base}/projects/p1/tasks/mf-todo01`)
      .flush({ message: 'nope' }, { status: 400, statusText: 'Bad' });
    mock.expectOne(`${base}/projects/p1/tasks`).flush({ items: TASKS });
    const toasts = TestBed.inject(ToastService).toasts();
    expect(toasts.some((t) => t.kind === 'error' && t.message.includes('nope'))).toBe(true);
  });

  it('backlog lists backlog tasks and "Move to To do" PATCHes status todo', () => {
    load();
    cmp.setTab('backlog');
    fixture.detectChanges();
    expect(keysIn('[data-testid="work-backlog"]')).toEqual(['mf-oper02', 'mf-back01']);
    (q('[data-key="mf-back01"] [data-testid="work-backlog-promote"]') as HTMLButtonElement).click();
    const req = mock.expectOne(`${base}/projects/p1/tasks/mf-back01`);
    expect(req.request.body).toEqual({ status: 'todo' });
    req.flush({ ...TASKS[5], status: 'todo' });
    mock.expectOne(`${base}/summary`).flush({ open_operator: 2, open_agent: 0, in_progress: 0 });
    mock.expectOne(`${base}/projects/p1/tasks`).flush({ items: TASKS });
  });

  it('opens on the tab named in ?tab=', () => {
    query = { tab: 'operator' };
    load();
    expect(cmp.tab()).toBe('operator');
    expect(q('[data-testid="work-operator-list"]')).toBeTruthy();
  });

  it('defaults a new task to the operator lane on the Operator tab', () => {
    load();
    cmp.setTab('operator');
    fixture.detectChanges();
    (q('[data-testid="work-new-task"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    const lane = q('[data-testid="work-new-lane"]') as HTMLSelectElement;
    expect(lane).toBeTruthy();
    fixture.detectChanges();
    expect(q('[data-testid="work-new-task-dialog"]')?.textContent).toContain('New operator task');
  });

  it('re-polls tasks every 20s while the page is visible', () => {
    vi.useFakeTimers();
    load();
    vi.advanceTimersByTime(20_000);
    mock.expectOne(`${base}/projects/p1/tasks`).flush({ items: TASKS });
    vi.advanceTimersByTime(20_000);
    mock.expectOne(`${base}/projects/p1/tasks`).flush({ items: TASKS });
  });

  it('shows a not-found message for a missing project', () => {
    fixture = TestBed.createComponent(WorkProjectBoardComponent);
    fixture.detectChanges();
    mock
      .expectOne(`${base}/projects/p1`)
      .flush({ message: 'not found' }, { status: 404, statusText: 'Not Found' });
    fixture.detectChanges();
    expect(q('[data-testid="work-project-error"]')?.textContent).toContain("doesn't exist");
  });
});
