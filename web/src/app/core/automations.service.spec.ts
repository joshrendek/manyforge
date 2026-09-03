import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AutomationGraph, AutomationsService } from './automations.service';

describe('AutomationsService', () => {
  let service: AutomationsService;
  let http: HttpTestingController;

  beforeEach(() => {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting()] });
    service = TestBed.inject(AutomationsService);
    http = TestBed.inject(HttpTestingController);
  });
  afterEach(() => http.verify());

  it('uses the business-scoped collection API', () => {
    service.list('b1', 'next').subscribe();
    const list = http.expectOne((request) => request.url === '/api/v1/businesses/b1/mailing/automations' && request.params.get('cursor') === 'next');
    expect(list.request.method).toBe('GET');
    list.flush({ items: [], next_cursor: null });

    service.create('b1', { name: 'Welcome', allow_reenroll: true }).subscribe();
    const create = http.expectOne('/api/v1/businesses/b1/mailing/automations');
    expect(create.request.method).toBe('POST');
    expect(create.request.body).toEqual({ name: 'Welcome', allow_reenroll: true });
    create.flush({});
  });

  it('puts the graph itself as the request body', () => {
    const graph: AutomationGraph = { nodes: [], edges: [] };
    service.putGraph('b1', 'a1', 'v1', graph).subscribe();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/versions/v1/graph');
    expect(request.request.method).toBe('PUT');
    expect(request.request.body).toBe(graph);
    request.flush({ graph });
  });
});
