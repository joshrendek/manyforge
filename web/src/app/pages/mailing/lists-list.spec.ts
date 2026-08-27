import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingListsListComponent } from './lists-list';

const businesses = {
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
const list = {
  id: 'l1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  slug: 'updates',
  name: 'Updates',
  description: null,
  double_opt_in: true,
  status: 'active',
  created_at: '',
  updated_at: '',
};

describe('MailingListsListComponent', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    http = TestBed.inject(HttpTestingController);
  });
  afterEach(() => {
    http.verify();
    localStorage.clear();
  });

  function mount(): ComponentFixture<MailingListsListComponent> {
    const fixture = TestBed.createComponent(MailingListsListComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush(businesses);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses/b1/mailing/lists').flush({
      items: [list],
      next_cursor: null,
    });
    fixture.detectChanges();
    return fixture;
  }

  it('renders lists linked to their detail route', () => {
    const fixture = mount();
    const link = fixture.nativeElement.querySelector(
      '[data-testid="mailing-list-open"]',
    ) as HTMLAnchorElement;
    expect(link.textContent).toContain('Updates');
    expect(link.getAttribute('href')).toBe('/mailing/b1/lists/l1');
  });

  it('creates a double-opt-in list and reloads', () => {
    const fixture = mount();
    fixture.componentInstance.newName = 'Newsletter';
    fixture.componentInstance.create();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/lists');
    expect(request.request.body).toEqual({ name: 'Newsletter', double_opt_in: true });
    request.flush({ ...list, id: 'l2', name: 'Newsletter', slug: 'newsletter' });
    http.expectOne('/api/v1/businesses/b1/mailing/lists').flush({
      items: [list],
      next_cursor: null,
    });
    fixture.detectChanges();
    expect(fixture.componentInstance.newName).toBe('');
  });
});
