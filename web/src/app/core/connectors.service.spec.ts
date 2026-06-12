import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { beforeEach, describe, expect, it } from 'vitest';
import { Connector, ConnectorsService } from './connectors.service';

function conn(over: Partial<Connector> = {}): Connector {
  return {
    id: 'c1', business_id: 'b1', type: 'jira', display_name: 'Acme', base_url: 'https://acme.atlassian.net',
    allow_private_base_url: false, config: {}, status: 'enabled', last_reconciled_at: null,
    created_at: '2026-06-12T00:00:00Z', updated_at: '2026-06-12T00:00:00Z',
    health: { state: 'healthy', linked_ticket_count: 0, pending_outbound_ops: 0, failed_outbound_ops: 0, last_error: null },
    ...over,
  };
}

describe('ConnectorsService', () => {
  let svc: ConnectorsService;
  let mock: HttpTestingController;
  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    svc = TestBed.inject(ConnectorsService);
    mock = TestBed.inject(HttpTestingController);
  });

  it('list() sets degradedCount to the non-healthy connectors', () => {
    svc.list('b1').subscribe();
    mock.expectOne('/api/v1/businesses/b1/connectors').flush({
      items: [conn(), conn({ id: 'c2', health: { state: 'degraded', linked_ticket_count: 0, pending_outbound_ops: 0, failed_outbound_ops: 2, last_error: 'boom' } }), conn({ id: 'c3', status: 'disabled', health: { state: 'disabled', linked_ticket_count: 0, pending_outbound_ops: 0, failed_outbound_ops: 0, last_error: null } })],
    });
    expect(svc.degradedCount()).toBe(2);
  });

  it('rotate() PUTs to the credential subpath', () => {
    svc.rotate('b1', 'c1', { email: 'a@b.c', api_token: 't', webhook_secret: 'w' }).subscribe();
    const req = mock.expectOne('/api/v1/businesses/b1/connectors/c1/credential');
    expect(req.request.method).toBe('PUT');
    req.flush(conn());
  });

  it('test() POSTs to the test subpath', () => {
    svc.test('b1', 'c1').subscribe();
    const req = mock.expectOne('/api/v1/businesses/b1/connectors/c1/test');
    expect(req.request.method).toBe('POST');
    req.flush({ ok: true, detail: 'ok' });
  });

  it('sync() POSTs to the sync subpath and returns status', () => {
    let result: { status: string } | undefined;
    svc.sync('b1', 'c1').subscribe((r) => (result = r));
    const req = mock.expectOne('/api/v1/businesses/b1/connectors/c1/sync');
    expect(req.request.method).toBe('POST');
    req.flush({ status: 'sync_started' });
    expect(result?.status).toBe('sync_started');
  });
});
