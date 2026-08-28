import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingService } from './mailing.service';

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
      .listSubscribers('b1', 'l1', {
        q: 'ada@example.com',
        status: 'active',
        tag: 'vip',
        cursor: 'next',
        limit: 25,
      })
      .subscribe();
    const request = http.expectOne(
      (candidate) => candidate.url === '/api/v1/businesses/b1/mailing/lists/l1/subscribers',
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
    service.importSubscribers('b1', 'l1', file, true, false).subscribe();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/lists/l1/subscribers/import');
    expect(request.request.body).toBeInstanceOf(FormData);
    const body = request.request.body as FormData;
    expect(body.get('file')).toBe(file);
    expect(body.get('consent_attested')).toBe('true');
    expect(body.get('skip_confirmation')).toBe('false');
    request.flush({ imported: 1, skipped: 0, errors: [] });
  });

  it('creates templates with the API snake_case body unchanged', () => {
    service
      .createTemplate('b1', {
        name: 'Welcome',
        subject: 'Hello',
        body_markdown: '# Hello',
        track_opens: true,
        track_clicks: false,
      })
      .subscribe();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/templates');
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
      .putSendingProfile('b1', {
        mode: 'resend',
        from_email: 'news@example.test',
        from_name: 'News',
        resend: { api_key: 're_secret' },
      })
      .subscribe();
    const put = http.expectOne('/api/v1/businesses/b1/mailing/sending-profile');
    expect(put.request.method).toBe('PUT');
    expect(put.request.body.resend.api_key).toBe('re_secret');
    put.flush({ has_credentials: true });

    service.verifySendingProfile('b1').subscribe();
    const verify = http.expectOne('/api/v1/businesses/b1/mailing/sending-profile/verify');
    expect(verify.request.method).toBe('POST');
    verify.flush({ status: 'verified', has_credentials: true });

    service.testSendingProfile('b1', 'operator@example.test').subscribe();
    const test = http.expectOne('/api/v1/businesses/b1/mailing/sending-profile/test-send');
    expect(test.request.method).toBe('POST');
    expect(test.request.body).toEqual({ to: 'operator@example.test' });
    test.flush(null);
  });
});
