import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MailingSuppressionListComponent } from './suppression-list';

const BUSINESS_ID = '11111111-1111-4111-8111-111111111111';
const SUPPRESSION_ID = '22222222-2222-4222-8222-222222222222';
const SECOND_SUPPRESSION_ID = '33333333-3333-4333-8333-333333333333';
const SUPPRESSIONS_URL = `/api/v1/businesses/${BUSINESS_ID}/mailing/suppressions`;

const business = {
  id: BUSINESS_ID,
  parent_id: null,
  tenant_root_id: BUSINESS_ID,
  name: 'Acme',
  status: 'active',
  is_tenant_root: true,
};
const suppression = {
  id: SUPPRESSION_ID,
  business_id: BUSINESS_ID,
  tenant_root_id: BUSINESS_ID,
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
        candidate.url === SUPPRESSIONS_URL &&
        candidate.params.get('limit') === '50',
    );
    request.flush({ items, next_cursor: nextCursor });
    fixture.detectChanges();
  }

  it('creates a manual suppression and prepends it to the list', () => {
    flushList();
    fixture.componentInstance.newEmail = 'manual@example.com';
    fixture.componentInstance.create();
    const request = http.expectOne(SUPPRESSIONS_URL);
    expect(request.request.method).toBe('POST');
    expect(request.request.body).toEqual({ email: 'manual@example.com', reason: 'manual' });
    request.flush({ ...suppression, id: SECOND_SUPPRESSION_ID, email: 'manual@example.com', reason: 'manual' });

    expect(fixture.componentInstance.items().map((item) => item.email)).toEqual([
      'manual@example.com',
      'blocked@example.com',
    ]);
  });

  it('requires inline confirmation before removing a suppression', () => {
    flushList();
    fixture.componentInstance.pendingDelete.set(SUPPRESSION_ID);
    fixture.componentInstance.remove(suppression);
    const request = http.expectOne(`${SUPPRESSIONS_URL}/${SUPPRESSION_ID}`);
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
        candidate.url === SUPPRESSIONS_URL &&
        candidate.params.get('cursor') === 'next',
    );
    request.flush({
      items: [{ ...suppression, id: SECOND_SUPPRESSION_ID, email: 'second@example.com' }],
      next_cursor: null,
    });
    expect(fixture.componentInstance.items().map((item) => item.id)).toEqual([
      SUPPRESSION_ID,
      SECOND_SUPPRESSION_ID,
    ]);
  });
});
