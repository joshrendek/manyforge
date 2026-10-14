import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { WorkTask } from '../../core/work.service';
import { WorkTaskDetailComponent } from './task-detail';
import { makeTask } from './work-fixtures';

const url = '/api/v1/businesses/b1/work/projects/p1/tasks/mf-oper01';

describe('WorkTaskDetailComponent', () => {
  let fixture: ComponentFixture<WorkTaskDetailComponent>;
  let cmp: WorkTaskDetailComponent;
  let mock: HttpTestingController;
  let saved: WorkTask[];

  const task = makeTask({
    key: 'mf-oper01',
    title: 'Rotate key',
    lane: 'operator',
    notes: '\n[2026-10-12T10:00:00Z agent] tried the API, needs a human',
    labels: ['billing'],
  });

  function mount(t: WorkTask = task): void {
    fixture = TestBed.createComponent(WorkTaskDetailComponent);
    cmp = fixture.componentInstance;
    fixture.componentRef.setInput('task', t);
    fixture.componentRef.setInput('businessId', 'b1');
    fixture.componentRef.setInput('projectId', 'p1');
    saved = [];
    cmp.saved.subscribe((x) => saved.push(x));
    fixture.detectChanges();
  }

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => mock.verify());

  it('shows the key, the operator lane, and notes as preformatted text', () => {
    mount();
    expect(q('[data-testid="work-task-dialog-key"]')?.textContent).toBe('mf-oper01');
    expect(q('[data-testid="work-task-dialog"]')?.textContent).toContain('Operator');
    const notes = q('[data-testid="work-task-notes"]');
    expect(notes?.tagName).toBe('PRE');
    expect(notes?.textContent).toContain('needs a human');
  });

  it('Save PATCHes only the fields that changed', () => {
    mount();
    expect(cmp.dirty()).toBe(false);
    cmp.draft.title = 'Rotate the live key';
    cmp.draft.priority = 0;
    cmp.draft.labels = ['billing', 'prod'];
    cmp.save();
    const req = mock.expectOne(url);
    expect(req.request.method).toBe('PATCH');
    expect(req.request.body).toEqual({
      title: 'Rotate the live key',
      priority: 0,
      labels: ['billing', 'prod'],
    });
    const updated = {
      ...task,
      title: 'Rotate the live key',
      priority: 0,
      labels: ['billing', 'prod'],
    };
    req.flush(updated);
    expect(saved).toEqual([updated]);
    expect(cmp.dirty()).toBe(false);
  });

  it('Add note PATCHes { note } and clears the box', () => {
    mount();
    cmp.noteText = '  done via dashboard  ';
    cmp.addNote();
    const req = mock.expectOne(url);
    expect(req.request.body).toEqual({ note: 'done via dashboard' });
    req.flush({ ...task, notes: task.notes + '\n[now you] done via dashboard' });
    expect(cmp.noteText).toBe('');
    expect(saved.length).toBe(1);
  });

  it('a background refresh updates untouched fields but keeps edits in progress', () => {
    mount();
    cmp.draft.title = 'My edit';
    fixture.componentRef.setInput('task', { ...task, status: 'in_progress', title: 'Agent title' });
    fixture.detectChanges();
    expect(cmp.draft.title).toBe('My edit');
    expect(cmp.draft.status).toBe('in_progress');
    expect(cmp.changes()).toEqual({ title: 'My edit' });
  });

  it('Escape closes the dialog', () => {
    mount();
    let closed = false;
    cmp.closed.subscribe(() => (closed = true));
    q('[data-testid="work-task-dialog"]')!.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }),
    );
    expect(closed).toBe(true);
  });
});
