import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter, Router } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AutomationsListComponent } from './automations-list';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const AUTOMATION_ID = '22222222-2222-4222-8222-222222222222';
const AUTOMATIONS_URL = `/api/v1/businesses/${BUSINESS_ID}/mailing/automations`;

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
    http.expectOne('/api/v1/businesses').flush({ items: [{ id: BUSINESS_ID, name: 'Acme', status: 'active', parent_id: null, tenant_root_id: BUSINESS_ID, is_tenant_root: true }] });
    http.expectOne(AUTOMATIONS_URL).flush({ items: [], next_cursor: null });
    fixture.componentInstance.newName = 'Welcome';
    fixture.componentInstance.allowReenroll = true;
    fixture.componentInstance.create();
    const request = http.expectOne(AUTOMATIONS_URL);
    expect(request.request.body).toEqual({ name: 'Welcome', allow_reenroll: true });
    request.flush({ id: AUTOMATION_ID });
    expect(navigate).toHaveBeenCalledWith(['/mailing', BUSINESS_ID, 'automations', AUTOMATION_ID]);
  });
});
