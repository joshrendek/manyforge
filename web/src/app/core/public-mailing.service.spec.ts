import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { PublicMailingService } from './public-mailing.service';

describe('PublicMailingService', () => {
  let service: PublicMailingService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    service = TestBed.inject(PublicMailingService);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  it('encodes the publishable key and sends the public subscription body', () => {
    service.subscribe('mlp/a', { email: 'ada@example.test', first_name: 'Ada' }).subscribe();
    const request = http.expectOne('/api/v1/mailing/public/mlp%2Fa/subscribe');
    expect(request.request.method).toBe('POST');
    expect(request.request.body).toEqual({ email: 'ada@example.test', first_name: 'Ada' });
    request.flush({ accepted: true });
  });

  it('treats root confirmation and unsubscribe responses as opaque text', () => {
    service.confirm('confirm/token').subscribe();
    const confirm = http.expectOne('/m/confirm/confirm%2Ftoken');
    expect(confirm.request.responseType).toBe('text');
    confirm.flush('<html>done</html>');

    service.unsubscribe('unsubscribe/token').subscribe();
    const unsubscribe = http.expectOne('/m/u/unsubscribe%2Ftoken');
    expect(unsubscribe.request.responseType).toBe('text');
    unsubscribe.flush('');
  });
});
