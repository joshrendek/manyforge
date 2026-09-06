import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter, Router } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MailingTemplatesListComponent } from './templates-list';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const TEMPLATE_ID = '22222222-2222-4222-8222-222222222222';
const TEMPLATES_URL = `/api/v1/businesses/${BUSINESS_ID}/mailing/templates`;

describe('MailingTemplatesListComponent', () => {
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

  it('creates an empty Markdown template then opens the editor', () => {
    const router = TestBed.inject(Router);
    const navigate = vi.spyOn(router, 'navigate').mockResolvedValue(true);
    const fixture = TestBed.createComponent(MailingTemplatesListComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({
      items: [
        {
          id: BUSINESS_ID,
          parent_id: null,
          tenant_root_id: BUSINESS_ID,
          name: 'Acme',
          status: 'active',
          is_tenant_root: true,
        },
      ],
      next_cursor: null,
    });
    http.expectOne(TEMPLATES_URL).flush({
      items: [],
      next_cursor: null,
    });
    fixture.componentInstance.newName = 'Welcome';
    fixture.componentInstance.newSubject = 'Hello';
    fixture.componentInstance.create();
    const request = http.expectOne(TEMPLATES_URL);
    expect(request.request.body).toMatchObject({
      name: 'Welcome',
      subject: 'Hello',
      body_markdown: '',
    });
    request.flush({ id: TEMPLATE_ID });
    expect(navigate).toHaveBeenCalledWith(['/mailing', BUSINESS_ID, 'templates', TEMPLATE_ID]);
  });
});
