import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ToastService } from '../../ui/toast/toast.service';
import { WorkProjectsListComponent } from './projects-list';
import { makeProject } from './work-fixtures';

const biz = {
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
  next_cursor: null,
};

describe('WorkProjectsListComponent', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => {
    mock.verify();
    localStorage.clear();
  });

  function mount(items = [makeProject()]): ComponentFixture<WorkProjectsListComponent> {
    const f = TestBed.createComponent(WorkProjectsListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/work/projects').flush({ items });
    f.detectChanges();
    return f;
  }

  it('renders a card per project with its board counts', () => {
    const el: HTMLElement = mount().nativeElement;
    const card = el.querySelector('[data-testid="work-project-card"]') as HTMLAnchorElement;
    expect(card.textContent).toContain('ManyForge');
    expect(card.getAttribute('href')).toBe('/work/b1/p1');
    expect(card.querySelector('[data-testid="count-todo"]')?.textContent?.trim()).toBe('2');
    expect(card.querySelector('[data-testid="count-operator"]')?.textContent?.trim()).toBe('1');
    expect(card.classList).toContain('has-asks');
  });

  it('shows the explanatory empty state when there are no projects', () => {
    const el: HTMLElement = mount([]).nativeElement;
    const empty = el.querySelector('[data-testid="work-projects-empty"]');
    expect(empty?.textContent).toContain('operator');
    expect(empty?.textContent).toContain('Connect');
  });

  it('creates a project, omitting blank optional fields, then reloads', () => {
    const f = mount([]);
    const toast = TestBed.inject(ToastService);
    f.componentInstance.newName = '  ManyForge ';
    f.componentInstance.newPrefix = 'mf';
    f.componentInstance.create();
    const req = mock.expectOne('/api/v1/businesses/b1/work/projects');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ name: 'ManyForge', key_prefix: 'mf' });
    req.flush(makeProject());
    mock.expectOne('/api/v1/businesses/b1/work/projects').flush({ items: [makeProject()] });
    expect(f.componentInstance.items().length).toBe(1);
    expect(toast.toasts().some((t) => t.message === 'Project created')).toBe(true);
  });

  it('reports a slug conflict on 409', () => {
    const f = mount([]);
    const toast = TestBed.inject(ToastService);
    f.componentInstance.newName = 'Dup';
    f.componentInstance.create();
    mock
      .expectOne('/api/v1/businesses/b1/work/projects')
      .flush({ message: 'exists' }, { status: 409, statusText: 'Conflict' });
    expect(toast.toasts().some((t) => t.message.includes('already exists'))).toBe(true);
    expect(f.componentInstance.creating()).toBe(false);
  });

  it('shows an error with retry when the list fails', () => {
    const f = TestBed.createComponent(WorkProjectsListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    mock
      .expectOne('/api/v1/businesses/b1/work/projects')
      .flush('x', { status: 500, statusText: 'err' });
    f.detectChanges();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('[data-testid="work-projects-error"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="work-projects-empty"]')).toBeNull();
  });
});
