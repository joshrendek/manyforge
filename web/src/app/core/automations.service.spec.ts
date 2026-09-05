import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AutomationGraph, AutomationsService } from './automations.service';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const AUTOMATION_ID = '88888888-8888-4888-8888-888888888888';
const VERSION_ID = '99999999-9999-4999-8999-999999999999';
const ENROLLMENT_ID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const BASE = `/api/v1/businesses/${BUSINESS_ID}/mailing/automations`;

describe('AutomationsService', () => {
  let service: AutomationsService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting()],
    });
    service = TestBed.inject(AutomationsService);
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  it('uses the business-scoped collection API', () => {
    service.list(BUSINESS_ID, 'next').subscribe();
    const list = http.expectOne(
      (request) => request.url === BASE && request.params.get('cursor') === 'next',
    );
    expect(list.request.method).toBe('GET');
    list.flush({ items: [], next_cursor: null });

    service.create(BUSINESS_ID, { name: 'Welcome', allow_reenroll: true }).subscribe();
    const create = http.expectOne(BASE);
    expect(create.request.method).toBe('POST');
    expect(create.request.body).toEqual({ name: 'Welcome', allow_reenroll: true });
    create.flush({});
  });

  it('puts the graph itself as the request body', () => {
    const graph: AutomationGraph = { nodes: [], edges: [] };
    service.putGraph(BUSINESS_ID, AUTOMATION_ID, VERSION_ID, graph).subscribe();
    const request = http.expectOne(`${BASE}/${AUTOMATION_ID}/versions/${VERSION_ID}/graph`);
    expect(request.request.method).toBe('PUT');
    expect(request.request.body).toBe(graph);
    request.flush({ graph });
  });

  it('rejects invalid UUIDs in every automation route position before HTTP dispatch', () => {
    const invalid = '../mailing/campaigns/66666666-6666-4666-8666-666666666666';
    const calls = [
      () => service.list(invalid),
      () => service.get(BUSINESS_ID, invalid),
      () => service.version(BUSINESS_ID, AUTOMATION_ID, invalid),
      () => service.exitEnrollment(BUSINESS_ID, AUTOMATION_ID, invalid),
    ];

    for (const call of calls) {
      expect(call).toThrowError('Invalid UUID route segment');
    }
    http.expectNone(() => true);
  });

  it('keeps canonical UUIDs on the original automation endpoint', () => {
    service
      .exitEnrollment(BUSINESS_ID, AUTOMATION_ID, ENROLLMENT_ID)
      .subscribe();

    const request = http.expectOne(
      `${BASE}/${AUTOMATION_ID}/enrollments/${ENROLLMENT_ID}/exit`,
    );
    expect(request.request.method).toBe('POST');
    request.flush({});
  });
});
