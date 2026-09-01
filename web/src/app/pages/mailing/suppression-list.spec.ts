import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingSuppressionListComponent } from './suppression-list';

const business = {
  id: 'b1',
  parent_id: null,
  tenant_root_id: 'b1',
  name: 'Acme',
  status: 'active',
  is_tenant_root: true,
};
const suppression = {
  id: 'sup1',
  business_id: 'b1',
  tenant_root_id: 'b1',
  email: 'blocked@example.com',
  reason: 'bounce' as const,
  source: 'resend',
  created_at: '2026-09-01T12:00:00Z',
};

describe('MailingSuppressionListComponent', () => {
  let http: HttpTestingController;
  let fixture: ComponentFixture<MailingSuppressionListComponent>;

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    http = TestBed.inject(HttpTestingController);
    fixture = TestBed.createComponent(MailingSuppressionListComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses').flush({ items: [business], next_cursor: null });
  });

  afterEach(() => {
    fixture.destroy();
    http.verify();
    localStorage.clear();
  });

  function flushList(items = [suppression], nextCursor: string | null = null): void {
    const request = http.expectOne(
      (candidate) =>
        candidate.url === '/api/v1/businesses/b1/mailing/suppressions' &&
        candidate.params.get('limit') === '50',
    );
    request.flush({ items, next_cursor: nextCursor });
    fixture.detectChanges();
  }

  it('creates a manual suppression and prepends it to the list', () => {
    flushList();
    fixture.componentInstance.newEmail = 'manual@example.com';
    fixture.componentInstance.create();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/suppressions');
    expect(request.request.method).toBe('POST');
    expect(request.request.body).toEqual({ email: 'manual@example.com', reason: 'manual' });
    request.flush({ ...suppression, id: 'sup2', email: 'manual@example.com', reason: 'manual' });

    expect(fixture.componentInstance.items().map((item) => item.email)).toEqual([
      'manual@example.com',
      'blocked@example.com',
    ]);
  });

  it('requires inline confirmation before removing a suppression', () => {
    flushList();
    fixture.componentInstance.pendingDelete.set('sup1');
    fixture.componentInstance.remove(suppression);
    const request = http.expectOne('/api/v1/businesses/b1/mailing/suppressions/sup1');
    expect(request.request.method).toBe('DELETE');
    request.flush(null);
    expect(fixture.componentInstance.items()).toEqual([]);
    expect(fixture.componentInstance.pendingDelete()).toBeNull();
  });

  it('appends the next suppression page', () => {
    flushList([suppression], 'next');
    fixture.componentInstance.loadMore();
    const request = http.expectOne(
      (candidate) =>
        candidate.url === '/api/v1/businesses/b1/mailing/suppressions' &&
        candidate.params.get('cursor') === 'next',
    );
    request.flush({
      items: [{ ...suppression, id: 'sup2', email: 'second@example.com' }],
      next_cursor: null,
    });
    expect(fixture.componentInstance.items().map((item) => item.id)).toEqual(['sup1', 'sup2']);
  });
});
