import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingListDetailComponent } from './list-detail';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const LIST_ID = '22222222-2222-4222-8222-222222222222';
const SUBSCRIBER_ID = '33333333-3333-4333-8333-333333333333';
const NEW_SUBSCRIBER_ID = '44444444-4444-4444-8444-444444444444';
const LIST_URL = `/api/v1/businesses/${BUSINESS_ID}/mailing/lists/${LIST_ID}`;

const list = {
  id: LIST_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  slug: 'updates',
  name: 'Updates',
  description: null,
  double_opt_in: true,
  status: 'active',
  created_at: '',
  updated_at: '',
};
const subscriber = {
  id: SUBSCRIBER_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  list_id: LIST_ID,
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
            snapshot: { paramMap: convertToParamMap({ businessId: BUSINESS_ID, listId: LIST_ID }) },
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
    http.expectOne(LIST_URL).flush(list);
    http.expectOne(`${LIST_URL}/keys`).flush({ items: [] });
    http
      .expectOne(`${LIST_URL}/subscribers`)
      .flush({ items: [subscriber], next_cursor: null });
    fixture.detectChanges();
    http.expectOne(`/api/v1/businesses/${BUSINESS_ID}/contacts`).flush({ items: [], next_cursor: null });
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
    const request = http.expectOne(`${LIST_URL}/subscribers`);
    expect(request.request.method).toBe('POST');
    expect(request.request.body).toMatchObject({
      email: 'grace@example.com',
      tags: ['customer'],
      skip_confirmation: false,
    });
    request.flush({ ...subscriber, id: NEW_SUBSCRIBER_ID, email: 'grace@example.com', tags: ['customer'] });
    http
      .expectOne(`${LIST_URL}/subscribers`)
      .flush({ items: [subscriber], next_cursor: null });
  });
});
