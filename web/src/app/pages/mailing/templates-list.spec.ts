import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter, Router } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { MailingTemplatesListComponent } from './templates-list';

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
          id: 'b1',
          parent_id: null,
          tenant_root_id: 'b1',
          name: 'Acme',
          status: 'active',
          is_tenant_root: true,
        },
      ],
      next_cursor: null,
    });
    http.expectOne('/api/v1/businesses/b1/mailing/templates').flush({
      items: [],
      next_cursor: null,
    });
    fixture.componentInstance.newName = 'Welcome';
    fixture.componentInstance.newSubject = 'Hello';
    fixture.componentInstance.create();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/templates');
    expect(request.request.body).toMatchObject({
      name: 'Welcome',
      subject: 'Hello',
      body_markdown: '',
    });
    request.flush({ id: 't1' });
    expect(navigate).toHaveBeenCalledWith(['/mailing', 'b1', 'templates', 't1']);
  });
});
