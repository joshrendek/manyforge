import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingService } from './mailing.service';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const LIST_ID = '22222222-2222-4222-8222-222222222222';
const SUBSCRIBER_ID = '33333333-3333-4333-8333-333333333333';
const KEY_ID = '44444444-4444-4444-8444-444444444444';
const TEMPLATE_ID = 'abcdefab-cdef-4abc-8def-abcdefabcdef';
const CAMPAIGN_ID = '66666666-6666-4666-8666-666666666666';
const SUPPRESSION_ID = '77777777-7777-4777-8777-777777777777';
const BASE = `/api/v1/businesses/${BUSINESS_ID}/mailing`;

describe('MailingService', () => {
  let service: MailingService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    service = TestBed.inject(MailingService);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  it('encodes subscriber filters as query parameters', () => {
    service
      .listSubscribers(BUSINESS_ID, LIST_ID, {
        q: 'ada@example.com',
        status: 'active',
        tag: 'vip',
        cursor: 'next',
        limit: 25,
      })
      .subscribe();
    const request = http.expectOne(
      (candidate) => candidate.url === `${BASE}/lists/${LIST_ID}/subscribers`,
    );
    expect(request.request.params.get('q')).toBe('ada@example.com');
    expect(request.request.params.get('status')).toBe('active');
    expect(request.request.params.get('tag')).toBe('vip');
    expect(request.request.params.get('cursor')).toBe('next');
    expect(request.request.params.get('limit')).toBe('25');
    request.flush({ items: [], next_cursor: null });
  });

  it('uses multipart FormData for consent-attested CSV imports', () => {
    const file = new File(['email\nada@example.com'], 'people.csv', { type: 'text/csv' });
    service.importSubscribers(BUSINESS_ID, LIST_ID, file, true, false).subscribe();
    const request = http.expectOne(`${BASE}/lists/${LIST_ID}/subscribers/import`);
    expect(request.request.body).toBeInstanceOf(FormData);
    const body = request.request.body as FormData;
    expect(body.get('file')).toBe(file);
    expect(body.get('consent_attested')).toBe('true');
    expect(body.get('skip_confirmation')).toBe('false');
    request.flush({ imported: 1, skipped: 0, errors: [] });
  });

  it('loads every mailing-list page for selectors', () => {
    let lists: string[] = [];
    service
      .listAllLists(BUSINESS_ID)
      .subscribe((items) => (lists = items.map((item) => item.id)));
    http
      .expectOne(`${BASE}/lists`)
      .flush({ items: [{ id: LIST_ID }], next_cursor: 'next' });
    const next = http.expectOne(
      (request) => request.url === `${BASE}/lists` && request.params.get('cursor') === 'next',
    );
    next.flush({ items: [{ id: TEMPLATE_ID }], next_cursor: null });
    expect(lists).toEqual([LIST_ID, TEMPLATE_ID]);
  });

  it('creates templates with the API snake_case body unchanged', () => {
    service
      .createTemplate(BUSINESS_ID, {
        name: 'Welcome',
        subject: 'Hello',
        body_markdown: '# Hello',
        track_opens: true,
        track_clicks: false,
      })
      .subscribe();
    const request = http.expectOne(`${BASE}/templates`);
    expect(request.request.method).toBe('POST');
    expect(request.request.body).toMatchObject({
      body_markdown: '# Hello',
      track_opens: true,
      track_clicks: false,
    });
    request.flush({});
  });

  it('keeps sending-profile credentials write-only and exposes verify/test actions', () => {
    service
      .putSendingProfile(BUSINESS_ID, {
        mode: 'resend',
        from_email: 'news@example.test',
        from_name: 'News',
        resend: { api_key: 're_secret' },
      })
      .subscribe();
    const put = http.expectOne(`${BASE}/sending-profile`);
    expect(put.request.method).toBe('PUT');
    expect(put.request.body.resend.api_key).toBe('re_secret');
    put.flush({ has_credentials: true });

    service.verifySendingProfile(BUSINESS_ID).subscribe();
    const verify = http.expectOne(`${BASE}/sending-profile/verify`);
    expect(verify.request.method).toBe('POST');
    verify.flush({ status: 'verified', has_credentials: true });

    service.testSendingProfile(BUSINESS_ID, 'operator@example.test').subscribe();
    const test = http.expectOne(`${BASE}/sending-profile/test-send`);
    expect(test.request.method).toBe('POST');
    expect(test.request.body).toEqual({ to: 'operator@example.test' });
    test.flush(null);
  });

  it('uses campaign preview and lifecycle endpoints with exact request bodies', () => {
    service
      .previewCampaign(BUSINESS_ID, {
        body_markdown: '# Hi',
        preheader: 'Preview',
        from_name: 'Acme',
      })
      .subscribe();
    const preview = http.expectOne(`${BASE}/campaigns/preview`);
    expect(preview.request.method).toBe('POST');
    expect(preview.request.body).toMatchObject({ body_markdown: '# Hi', from_name: 'Acme' });
    preview.flush({ html: '<p>Hi</p>', text: 'Hi' });

    service
      .testCampaign(BUSINESS_ID, CAMPAIGN_ID, ['ada@example.com', 'grace@example.com'])
      .subscribe();
    const test = http.expectOne(`${BASE}/campaigns/${CAMPAIGN_ID}/test-send`);
    expect(test.request.body).toEqual({ to: ['ada@example.com', 'grace@example.com'] });
    test.flush(null);

    service.sendCampaign(BUSINESS_ID, CAMPAIGN_ID, '2026-09-01T16:00:00.000Z').subscribe();
    const schedule = http.expectOne(`${BASE}/campaigns/${CAMPAIGN_ID}/send`);
    expect(schedule.request.body).toEqual({ scheduled_at: '2026-09-01T16:00:00.000Z' });
    schedule.flush({ status: 'scheduled' });

    service.cancelCampaign(BUSINESS_ID, CAMPAIGN_ID).subscribe();
    const cancel = http.expectOne(`${BASE}/campaigns/${CAMPAIGN_ID}/cancel`);
    expect(cancel.request.body).toEqual({});
    cancel.flush({ status: 'cancelled' });
  });

  it('uses the distinct template preview endpoint', () => {
    service.previewTemplate(BUSINESS_ID, { body_markdown: '# Template' }).subscribe();
    const request = http.expectOne(`${BASE}/templates/preview`);
    expect(request.request.method).toBe('POST');
    request.flush({ html: '<h1>Template</h1>', text: 'Template' });
  });

  it('loads campaign reporting and delivery pages with filters', () => {
    service.getCampaignStats(BUSINESS_ID, CAMPAIGN_ID).subscribe();
    http
      .expectOne(`${BASE}/campaigns/${CAMPAIGN_ID}/stats`)
      .flush({ campaign: { id: CAMPAIGN_ID }, links: [] });

    service
      .listCampaignDeliveries(BUSINESS_ID, CAMPAIGN_ID, {
        status: 'bounced',
        cursor: 'next',
        limit: 25,
      })
      .subscribe();
    const deliveries = http.expectOne(
      (request) => request.url === `${BASE}/campaigns/${CAMPAIGN_ID}/deliveries`,
    );
    expect(deliveries.request.params.get('status')).toBe('bounced');
    expect(deliveries.request.params.get('cursor')).toBe('next');
    expect(deliveries.request.params.get('limit')).toBe('25');
    deliveries.flush({ items: [], next_cursor: null });
  });

  it('creates, lists, and deletes tenant suppressions', () => {
    service.listSuppressions(BUSINESS_ID, 'next', 25).subscribe();
    const list = http.expectOne((request) => request.url === `${BASE}/suppressions`);
    expect(list.request.params.get('cursor')).toBe('next');
    expect(list.request.params.get('limit')).toBe('25');
    list.flush({ items: [], next_cursor: null });

    service.createSuppression(BUSINESS_ID, 'blocked@example.com').subscribe();
    const create = http.expectOne(`${BASE}/suppressions`);
    expect(create.request.method).toBe('POST');
    expect(create.request.body).toEqual({ email: 'blocked@example.com', reason: 'manual' });
    create.flush({ id: SUPPRESSION_ID });

    service.deleteSuppression(BUSINESS_ID, SUPPRESSION_ID).subscribe();
    const remove = http.expectOne(`${BASE}/suppressions/${SUPPRESSION_ID}`);
    expect(remove.request.method).toBe('DELETE');
    remove.flush(null);
  });

  // MF-WEB-013-001: Angular route parameters are decoded before service use.
  // The client must reject the decoded traversal before creating an HTTP request.
  it('rejects a decoded template ID before a campaign DELETE target is dispatched', () => {
    const decodedRouteID = decodeURIComponent(`..%2Fcampaigns%2F${CAMPAIGN_ID}`);

    expect(() => service.deleteTemplate(BUSINESS_ID, decodedRouteID)).toThrowError(
      'Invalid UUID route segment',
    );
    http.expectNone(() => true);
  });

  const invalidRouteIDs = [
    ['slash', `${TEMPLATE_ID}/campaigns/${CAMPAIGN_ID}`],
    ['backslash', `${TEMPLATE_ID}\\campaigns\\${CAMPAIGN_ID}`],
    ['dot segment', '..'],
    ['query', `${TEMPLATE_ID}?target=${CAMPAIGN_ID}`],
    ['fragment', `${TEMPLATE_ID}#${CAMPAIGN_ID}`],
    ['encoded separator', `${TEMPLATE_ID}%2Fcampaigns%2F${CAMPAIGN_ID}`],
    ['uppercase', TEMPLATE_ID.toUpperCase()],
    ['malformed', 'not-a-uuid'],
  ] as const;

  for (const [name, routeID] of invalidRouteIDs) {
    it(`rejects a ${name} route ID before HTTP dispatch`, () => {
      expect(() => service.deleteTemplate(BUSINESS_ID, routeID)).toThrowError(
        'Invalid UUID route segment',
      );
      http.expectNone(() => true);
    });
  }

  it('rejects invalid UUIDs in every mailing route position before HTTP dispatch', () => {
    const invalid = 'not-a-uuid';
    const calls = [
      () => service.listLists(invalid),
      () => service.getList(BUSINESS_ID, invalid),
      () => service.updateSubscriber(BUSINESS_ID, LIST_ID, invalid, {}),
      () => service.revokeKey(BUSINESS_ID, LIST_ID, invalid),
      () => service.getTemplate(BUSINESS_ID, invalid),
      () => service.getCampaign(BUSINESS_ID, invalid),
      () => service.deleteSuppression(BUSINESS_ID, invalid),
    ];

    for (const call of calls) {
      expect(call).toThrowError('Invalid UUID route segment');
    }
    http.expectNone(() => true);
  });

  it('keeps canonical UUIDs on the original template endpoint', () => {
    service.getTemplate(BUSINESS_ID, TEMPLATE_ID).subscribe();

    const request = http.expectOne(`${BASE}/templates/${TEMPLATE_ID}`);
    expect(request.request.method).toBe('GET');
    request.flush({ id: TEMPLATE_ID });
  });
});
