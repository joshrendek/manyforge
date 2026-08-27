import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingListDetailComponent } from './list-detail';

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
const subscriber = {
  id: 's1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  list_id: 'l1',
  email: 'ada@example.com',
  first_name: 'Ada',
  last_name: 'Lovelace',
  attributes: {},
  status: 'active',
  contact_id: null,
  consent_source: 'manual',
  consent_attested_by: null,
  consent_at: '',
  confirmed_at: '',
  unsubscribed_at: null,
  status_reason: null,
  tags: ['vip'],
  created_at: '',
  updated_at: '',
};

describe('MailingListDetailComponent', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([
          { path: 'mailing/:businessId/lists/:listId', component: MailingListDetailComponent },
        ]),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: { paramMap: convertToParamMap({ businessId: 'b1', listId: 'l1' }) },
          },
        },
      ],
    });
    http = TestBed.inject(HttpTestingController);
  });
  afterEach(() => http.verify());

  function mount(): ComponentFixture<MailingListDetailComponent> {
    const fixture = TestBed.createComponent(MailingListDetailComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses/b1/mailing/lists/l1').flush(list);
    http.expectOne('/api/v1/businesses/b1/mailing/lists/l1/keys').flush({ items: [] });
    http
      .expectOne('/api/v1/businesses/b1/mailing/lists/l1/subscribers')
      .flush({ items: [subscriber], next_cursor: null });
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses/b1/contacts').flush({ items: [], next_cursor: null });
    fixture.detectChanges();
    return fixture;
  }

  it('renders subscriber status and tags', () => {
    const fixture = mount();
    const row: HTMLElement = fixture.nativeElement.querySelector('[data-testid="subscriber-row"]');
    expect(row.textContent).toContain('ada@example.com');
    expect(row.textContent).toContain('active');
    expect(row.textContent).toContain('vip');
  });

  it('adds a tagged subscriber and reloads the table', () => {
    const fixture = mount();
    fixture.componentInstance.newEmail = 'grace@example.com';
    fixture.componentInstance.newTags = ['customer'];
    fixture.componentInstance.addSubscriber();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/lists/l1/subscribers');
    expect(request.request.method).toBe('POST');
    expect(request.request.body).toMatchObject({
      email: 'grace@example.com',
      tags: ['customer'],
      skip_confirmation: false,
    });
    request.flush({ ...subscriber, id: 's2', email: 'grace@example.com', tags: ['customer'] });
    http
      .expectOne('/api/v1/businesses/b1/mailing/lists/l1/subscribers')
      .flush({ items: [subscriber], next_cursor: null });
  });
});
