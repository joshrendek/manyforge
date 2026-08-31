import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter, Router } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MailingCampaignsListComponent } from './campaigns-list';

const business = {
  id: 'b1',
  parent_id: null,
  tenant_root_id: 'b1',
  name: 'Acme',
  status: 'active',
  is_tenant_root: true,
};
const list = {
  id: 'l1',
  business_id: 'b1',
  tenant_root_id: 'b1',
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
    http.expectOne('/api/v1/businesses/b1/mailing/lists').flush({
      items: [list],
      next_cursor: null,
    });
    http.expectOne('/api/v1/businesses/b1/mailing/campaigns').flush({
      items: [],
      next_cursor: null,
    });

    fixture.componentInstance.newName = 'September update';
    fixture.componentInstance.create();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/campaigns');
    expect(request.request.method).toBe('POST');
    expect(request.request.body).toEqual({
      list_id: 'l1',
      name: 'September update',
      subject: '',
      body_markdown: '',
      tag_filter: [],
      track_opens: true,
      track_clicks: true,
    });
    request.flush({ id: 'c1' });
    expect(navigate).toHaveBeenCalledWith(['/mailing', 'b1', 'campaigns', 'c1']);
  });

  it('appends cursor-paginated campaigns', () => {
    const fixture = TestBed.createComponent(MailingCampaignsListComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({ items: [business], next_cursor: null });
    http.expectOne('/api/v1/businesses/b1/mailing/lists').flush({
      items: [list],
      next_cursor: null,
    });
    http.expectOne('/api/v1/businesses/b1/mailing/campaigns').flush({
      items: [{ id: 'c1', name: 'One', subject: '', status: 'draft', updated_at: '' }],
      next_cursor: 'next',
    });
    fixture.componentInstance.loadMore();
    const next = http.expectOne(
      (request) =>
        request.url === '/api/v1/businesses/b1/mailing/campaigns' &&
        request.params.get('cursor') === 'next',
    );
    next.flush({
      items: [{ id: 'c2', name: 'Two', subject: '', status: 'sent', updated_at: '' }],
      next_cursor: null,
    });
    expect(fixture.componentInstance.items().map((campaign) => campaign.id)).toEqual(['c1', 'c2']);
  });

  it('loads a newly selected business while the previous request is still in flight', () => {
    const fixture = TestBed.createComponent(MailingCampaignsListComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({
      items: [business, { ...business, id: 'b2', tenant_root_id: 'b2', name: 'Beta' }],
      next_cursor: null,
    });
    const oldLists = http.expectOne('/api/v1/businesses/b1/mailing/lists');
    const oldCampaigns = http.expectOne('/api/v1/businesses/b1/mailing/campaigns');

    fixture.componentInstance.selectBusiness('b2');
    const newLists = http.expectOne('/api/v1/businesses/b2/mailing/lists');
    const newCampaigns = http.expectOne('/api/v1/businesses/b2/mailing/campaigns');
    oldLists.flush({ items: [list], next_cursor: null });
    oldCampaigns.flush({ items: [{ id: 'old', name: 'Old' }], next_cursor: null });
    newLists.flush({ items: [{ ...list, id: 'l2', business_id: 'b2' }], next_cursor: null });
    newCampaigns.flush({ items: [{ id: 'new', name: 'New' }], next_cursor: null });

    expect(fixture.componentInstance.items().map((campaign) => campaign.id)).toEqual(['new']);
    expect(fixture.componentInstance.loading()).toBe(false);
  });

  it('ignores the original response after switching away and back to a business', () => {
    const fixture = TestBed.createComponent(MailingCampaignsListComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({
      items: [business, { ...business, id: 'b2', tenant_root_id: 'b2', name: 'Beta' }],
      next_cursor: null,
    });
    fixture.componentInstance.selectBusiness('b2');
    fixture.componentInstance.selectBusiness('b1');

    const b1Lists = http.match('/api/v1/businesses/b1/mailing/lists');
    const b1Campaigns = http.match('/api/v1/businesses/b1/mailing/campaigns');
    const b2Lists = http.expectOne('/api/v1/businesses/b2/mailing/lists');
    const b2Campaigns = http.expectOne('/api/v1/businesses/b2/mailing/campaigns');
    expect(b1Lists).toHaveLength(2);
    expect(b1Campaigns).toHaveLength(2);

    b1Lists[0].flush({ items: [{ ...list, id: 'old-list' }], next_cursor: null });
    b1Campaigns[0].flush({ items: [{ id: 'old', name: 'Old' }], next_cursor: null });
    b2Lists.flush({ items: [], next_cursor: null });
    b2Campaigns.flush({ items: [], next_cursor: null });
    b1Lists[1].flush({ items: [list], next_cursor: null });
    b1Campaigns[1].flush({ items: [{ id: 'new', name: 'New' }], next_cursor: null });

    expect(fixture.componentInstance.items().map((campaign) => campaign.id)).toEqual(['new']);
    expect(fixture.componentInstance.lists().map((item) => item.id)).toEqual(['l1']);
  });
});
