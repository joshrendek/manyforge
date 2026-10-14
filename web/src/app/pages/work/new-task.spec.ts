import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { WorkTask } from '../../core/work.service';
import { WorkNewTaskComponent } from './new-task';
import { makeTask } from './work-fixtures';

const url = '/api/v1/businesses/b1/work/projects/p1/tasks';

describe('WorkNewTaskComponent', () => {
  let fixture: ComponentFixture<WorkNewTaskComponent>;
  let cmp: WorkNewTaskComponent;
  let mock: HttpTestingController;

  function mount(lane: 'agent' | 'operator' = 'agent', status: 'todo' | 'backlog' = 'todo'): void {
    fixture = TestBed.createComponent(WorkNewTaskComponent);
    cmp = fixture.componentInstance;
    fixture.componentRef.setInput('businessId', 'b1');
    fixture.componentRef.setInput('projectId', 'p1');
    fixture.componentRef.setInput('defaultLane', lane);
    fixture.componentRef.setInput('defaultStatus', status);
    fixture.detectChanges();
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => mock.verify());

  it('posts an operator task with the chosen fields and emits the created task', () => {
    mount('operator');
    const created: WorkTask[] = [];
    cmp.created.subscribe((t) => created.push(t));
    expect(cmp.lane).toBe('operator');
    cmp.title = ' Approve DNS ';
    cmp.description = 'Add the TXT record';
    cmp.priority = 1;
    cmp.submit();
    const req = mock.expectOne(url);
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({
      title: 'Approve DNS',
      description: 'Add the TXT record',
      lane: 'operator',
      status: 'todo',
      priority: 1,
      type: 'task',
    });
    const t = makeTask({ key: 'mf-new001', lane: 'operator' });
    req.flush(t);
    expect(created).toEqual([t]);
  });

  it('defaults to the backlog when opened from the Backlog tab and omits a blank description', () => {
    mount('agent', 'backlog');
    cmp.title = 'Later';
    cmp.submit();
    const req = mock.expectOne(url);
    expect(req.request.body.status).toBe('backlog');
    expect('description' in req.request.body).toBe(false);
    req.flush(makeTask());
  });

  it('does not submit without a title', () => {
    mount();
    cmp.title = '   ';
    cmp.submit();
    mock.expectNone(url);
  });
});
