import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter, Router } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AutomationsListComponent } from './automations-list';

describe('AutomationsListComponent', () => {
  let http: HttpTestingController;

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])] });
    http = TestBed.inject(HttpTestingController);
  });
  afterEach(() => { http.verify(); localStorage.clear(); });

  it('creates a draft and opens its editor', () => {
    const router = TestBed.inject(Router);
    const navigate = vi.spyOn(router, 'navigate').mockResolvedValue(true);
    const fixture = TestBed.createComponent(AutomationsListComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({ items: [{ id: 'b1', name: 'Acme', status: 'active', parent_id: null, tenant_root_id: 'b1', is_tenant_root: true }] });
    http.expectOne('/api/v1/businesses/b1/mailing/automations').flush({ items: [], next_cursor: null });
    fixture.componentInstance.newName = 'Welcome';
    fixture.componentInstance.allowReenroll = true;
    fixture.componentInstance.create();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/automations');
    expect(request.request.body).toEqual({ name: 'Welcome', allow_reenroll: true });
    request.flush({ id: 'a1' });
    expect(navigate).toHaveBeenCalledWith(['/mailing', 'b1', 'automations', 'a1']);
  });
});
