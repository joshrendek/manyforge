import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingCampaignStatsComponent } from './campaign-stats';

const campaign = {
  id: 'c1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  list_id: 'l1',
  profile_id: 'p1',
  name: 'September update',
  subject: 'What is new',
  preheader: null,
  body_markdown: '# Hello',
  tag_filter: [],
  track_opens: true,
  track_clicks: true,
  status: 'sent',
  scheduled_at: null,
  started_at: '2026-09-01T12:00:00Z',
  completed_at: '2026-09-01T12:05:00Z',
  recipient_count: 100,
  sent_count: 98,
  delivered_count: 96,
  bounced_count: 2,
  complained_count: 1,
  opened_count: 48,
  clicked_count: 24,
  unsubscribed_count: 3,
  failed_count: 2,
  last_error: null,
  created_by: 'u1',
  created_at: '2026-09-01T11:00:00Z',
  updated_at: '2026-09-01T12:05:00Z',
};

const delivery = {
  id: 'd1',
  campaign_id: 'c1',
  subscriber_id: 's1',
  email: 'ada@example.com',
  status: 'delivered',
  attempts: 1,
  not_before: '2026-09-01T12:00:00Z',
  lease_until: null,
  message_id: 'message-1',
  provider_message_id: 'provider-1',
  opened_at: '2026-09-01T12:02:00Z',
  first_clicked_at: '2026-09-01T12:03:00Z',
  last_error: null,
  created_at: '2026-09-01T12:00:00Z',
};

describe('MailingCampaignStatsComponent', () => {
  let http: HttpTestingController;
  let fixture: ComponentFixture<MailingCampaignStatsComponent>;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: { paramMap: convertToParamMap({ businessId: 'b1', campaignId: 'c1' }) },
          },
        },
      ],
    });
    http = TestBed.inject(HttpTestingController);
    fixture = TestBed.createComponent(MailingCampaignStatsComponent);
    fixture.detectChanges();
  });

  afterEach(() => {
    fixture.destroy();
    http.verify();
  });

  function flushInitial(nextCursor: string | null = 'next'): void {
    http.expectOne('/api/v1/businesses/b1/mailing/campaigns/c1/stats').flush({
      campaign,
      links: [{ url: 'https://example.com/docs', click_count: 30, unique_click_count: 24 }],
    });
    const deliveries = http.expectOne(
      (request) =>
        request.url === '/api/v1/businesses/b1/mailing/campaigns/c1/deliveries' &&
        request.params.get('limit') === '50' &&
        !request.params.has('status'),
    );
    deliveries.flush({ items: [delivery], next_cursor: nextCursor });
    fixture.detectChanges();
  }

  it('renders campaign counters, link totals, and delivery engagement', () => {
    flushInitial();

    expect(fixture.nativeElement.querySelector('[data-testid="stat-delivered"]').textContent).toBe(
      '96',
    );
    expect(fixture.nativeElement.textContent).toContain('96.0% of recipients');
    expect(fixture.nativeElement.textContent).toContain('https://example.com/docs');
    expect(fixture.nativeElement.textContent).toContain('ada@example.com');
    expect(fixture.nativeElement.textContent).toContain('Clicked');
  });

  it('appends cursor pages and resets results when filtering by status', () => {
    flushInitial();
    fixture.componentInstance.loadMore();
    const next = http.expectOne(
      (request) =>
        request.url === '/api/v1/businesses/b1/mailing/campaigns/c1/deliveries' &&
        request.params.get('cursor') === 'next',
    );
    next.flush({
      items: [{ ...delivery, id: 'd2', email: 'grace@example.com' }],
      next_cursor: null,
    });
    expect(fixture.componentInstance.deliveries().map((item) => item.id)).toEqual(['d1', 'd2']);

    fixture.componentInstance.setDeliveryStatus('bounced');
    const filtered = http.expectOne(
      (request) =>
        request.url === '/api/v1/businesses/b1/mailing/campaigns/c1/deliveries' &&
        request.params.get('status') === 'bounced' &&
        !request.params.has('cursor'),
    );
    filtered.flush({ items: [{ ...delivery, id: 'd3', status: 'bounced' }], next_cursor: null });
    expect(fixture.componentInstance.deliveries().map((item) => item.id)).toEqual(['d3']);
  });
});
