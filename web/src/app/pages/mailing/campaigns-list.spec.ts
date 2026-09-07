import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter, Router } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MailingCampaignsListComponent } from './campaigns-list';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const SECOND_BUSINESS_ID = '22222222-2222-4222-8222-222222222222';
const LIST_ID = '33333333-3333-4333-8333-333333333333';
const SECOND_LIST_ID = '44444444-4444-4444-8444-444444444444';
const CAMPAIGN_ID = '55555555-5555-4555-8555-555555555555';
const SECOND_CAMPAIGN_ID = '66666666-6666-4666-8666-666666666666';
const STALE_CAMPAIGN_ID = '77777777-7777-4777-8777-777777777777';
const FRESH_CAMPAIGN_ID = '88888888-8888-4888-8888-888888888888';
const BASE = `/api/v1/businesses/${BUSINESS_ID}/mailing`;
const SECOND_BASE = `/api/v1/businesses/${SECOND_BUSINESS_ID}/mailing`;

const business = {
  id: BUSINESS_ID,
  parent_id: null,
  tenant_root_id: BUSINESS_ID,
  name: 'Acme',
  status: 'active',
  is_tenant_root: true,
};
const list = {
  id: LIST_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
  slug: 'news',
  name: 'News',
  description: null,
  double_opt_in: true,
  status: 'active',
  created_at: '',
  updated_at: '',
};

describe('MailingCampaignsListComponent', () => {
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

  it('loads list choices and creates an empty draft before opening it', () => {
    const router = TestBed.inject(Router);
    const navigate = vi.spyOn(router, 'navigate').mockResolvedValue(true);
    const fixture = TestBed.createComponent(MailingCampaignsListComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({ items: [business], next_cursor: null });
    http.expectOne(`${BASE}/lists`).flush({
      items: [list],
      next_cursor: null,
    });
    http.expectOne(`${BASE}/campaigns`).flush({
      items: [],
      next_cursor: null,
    });

    fixture.componentInstance.newName = 'September update';
    fixture.componentInstance.create();
    const request = http.expectOne(`${BASE}/campaigns`);
    expect(request.request.method).toBe('POST');
    expect(request.request.body).toEqual({
      list_id: LIST_ID,
      name: 'September update',
      subject: '',
      body_markdown: '',
      tag_filter: [],
      track_opens: true,
      track_clicks: true,
    });
    request.flush({ id: CAMPAIGN_ID });
    expect(navigate).toHaveBeenCalledWith(['/mailing', BUSINESS_ID, 'campaigns', CAMPAIGN_ID]);
  });

  it('appends cursor-paginated campaigns', () => {
    const fixture = TestBed.createComponent(MailingCampaignsListComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({ items: [business], next_cursor: null });
    http.expectOne(`${BASE}/lists`).flush({
      items: [list],
      next_cursor: null,
    });
    http.expectOne(`${BASE}/campaigns`).flush({
      items: [{ id: CAMPAIGN_ID, name: 'One', subject: '', status: 'draft', updated_at: '' }],
      next_cursor: 'next',
    });
    fixture.componentInstance.loadMore();
    const next = http.expectOne(
      (request) =>
        request.url === `${BASE}/campaigns` &&
        request.params.get('cursor') === 'next',
    );
    next.flush({
      items: [{ id: SECOND_CAMPAIGN_ID, name: 'Two', subject: '', status: 'sent', updated_at: '' }],
      next_cursor: null,
    });
    expect(fixture.componentInstance.items().map((campaign) => campaign.id)).toEqual([
      CAMPAIGN_ID,
      SECOND_CAMPAIGN_ID,
    ]);
  });

  it('loads a newly selected business while the previous request is still in flight', () => {
    const fixture = TestBed.createComponent(MailingCampaignsListComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({
      items: [business, { ...business, id: SECOND_BUSINESS_ID, tenant_root_id: SECOND_BUSINESS_ID, name: 'Beta' }],
      next_cursor: null,
    });
    const oldLists = http.expectOne(`${BASE}/lists`);
    const oldCampaigns = http.expectOne(`${BASE}/campaigns`);

    fixture.componentInstance.selectBusiness(SECOND_BUSINESS_ID);
    const newLists = http.expectOne(`${SECOND_BASE}/lists`);
    const newCampaigns = http.expectOne(`${SECOND_BASE}/campaigns`);
    expect(oldLists.cancelled).toBe(true);
    expect(oldCampaigns.cancelled).toBe(true);
    newLists.flush({ items: [{ ...list, id: SECOND_LIST_ID, business_id: SECOND_BUSINESS_ID }], next_cursor: null });
    newCampaigns.flush({ items: [{ id: FRESH_CAMPAIGN_ID, name: 'New' }], next_cursor: null });

    expect(fixture.componentInstance.items().map((campaign) => campaign.id)).toEqual([FRESH_CAMPAIGN_ID]);
    expect(fixture.componentInstance.loading()).toBe(false);
  });

  it('cancels obsolete requests after switching away and back to a business', () => {
    const fixture = TestBed.createComponent(MailingCampaignsListComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({
      items: [business, { ...business, id: SECOND_BUSINESS_ID, tenant_root_id: SECOND_BUSINESS_ID, name: 'Beta' }],
      next_cursor: null,
    });
    fixture.componentInstance.selectBusiness(SECOND_BUSINESS_ID);
    fixture.componentInstance.selectBusiness(BUSINESS_ID);

    const b1Lists = http.match(`${BASE}/lists`);
    const b1Campaigns = http.match(`${BASE}/campaigns`);
    const b2Lists = http.expectOne(`${SECOND_BASE}/lists`);
    const b2Campaigns = http.expectOne(`${SECOND_BASE}/campaigns`);
    expect(b1Lists).toHaveLength(2);
    expect(b1Campaigns).toHaveLength(2);

    expect(b1Lists[0].cancelled).toBe(true);
    expect(b1Campaigns[0].cancelled).toBe(true);
    expect(b2Lists.cancelled).toBe(true);
    expect(b2Campaigns.cancelled).toBe(true);
    b1Lists[1].flush({ items: [list], next_cursor: null });
    b1Campaigns[1].flush({ items: [{ id: FRESH_CAMPAIGN_ID, name: 'New' }], next_cursor: null });

    expect(fixture.componentInstance.items().map((campaign) => campaign.id)).toEqual([FRESH_CAMPAIGN_ID]);
    expect(fixture.componentInstance.lists().map((item) => item.id)).toEqual([LIST_ID]);
  });
});
