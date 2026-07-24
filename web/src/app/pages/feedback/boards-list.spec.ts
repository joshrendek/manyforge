import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ToastService } from '../../ui/toast/toast.service';
import { FeedbackBoardsListComponent } from './boards-list';

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
const boards = {
  items: [
    {
      id: 'bd1',
      business_id: 'b1',
      tenant_root_id: 'b1',
      slug: 'mobile-app',
      name: 'Mobile App',
      description: null,
      is_public: true,
      created_at: '',
      updated_at: '',
    },
  ],
  next_cursor: null,
};

describe('FeedbackBoardsListComponent', () => {
  let mock: HttpTestingController;
  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => {
    document.documentElement.setAttribute('data-theme', 'light');
    localStorage.clear();
  });

  function mount(): ComponentFixture<FeedbackBoardsListComponent> {
    const f = TestBed.createComponent(FeedbackBoardsListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/feedback/boards').flush(boards);
    f.detectChanges();
    return f;
  }

  it('loads businesses then lists boards', () => {
    const f = mount();
    expect(f.componentInstance.items().length).toBe(1);
    expect(f.componentInstance.items()[0].name).toBe('Mobile App');
  });

  it('renders a board row with name, slug, and a Public pill', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('[data-testid="board-row"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="board-name-cell"]')?.textContent).toContain(
      'Mobile App',
    );
    expect(el.querySelector('[data-testid="board-slug-cell"]')?.textContent).toContain(
      'mobile-app',
    );
    expect(el.querySelector('[data-testid="board-visibility-cell"]')?.textContent).toContain(
      'Public',
    );
  });

  it('shows the empty state when there are no boards', () => {
    const f = TestBed.createComponent(FeedbackBoardsListComponent);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/feedback/boards').flush({ items: [], next_cursor: null });
    f.detectChanges();
    expect(f.nativeElement.querySelector('[data-testid="boards-empty"]')).toBeTruthy();
  });

  it('creates a board via the inline form then reloads and toasts', () => {
    const f = mount();
    const toastSvc = TestBed.inject(ToastService);
    f.componentInstance.newName = 'Roadmap';
    f.componentInstance.newPublic = true;
    f.componentInstance.create();
    const req = mock.expectOne('/api/v1/businesses/b1/feedback/boards');
    expect(req.request.body).toEqual({ name: 'Roadmap', is_public: true });
    req.flush({
      id: 'bd2',
      business_id: 'b1',
      tenant_root_id: 'b1',
      slug: 'roadmap',
      name: 'Roadmap',
      description: null,
      is_public: true,
      created_at: '',
      updated_at: '',
    });
    f.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/feedback/boards').flush(boards); // reload
    f.detectChanges();
    expect(toastSvc.toasts().some((t) => t.message.includes('Board created'))).toBe(true);
    expect(f.componentInstance.newName).toBe('');
  });

  it('links each row to the board detail route', () => {
    const el: HTMLElement = mount().nativeElement;
    const link = el.querySelector('[data-testid="board-row"] a') as HTMLAnchorElement;
    expect(link?.getAttribute('href')).toBe('/feedback/b1/bd1');
  });
});
