import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AnalyticsSitesListComponent } from './sites-list';

const biz = {
  items: [
    {
      id: 'b1',
      parent_id: null,
      tenant_root_id: 'b1',
      name: 'Acme',
      status: 'active',
      is_tenant_root: true,
    },
    {
      id: 'b2',
      parent_id: null,
      tenant_root_id: 'b2',
      name: 'Acme Labs',
      status: 'active',
      is_tenant_root: true,
    },
  ],
  next_cursor: null,
};

const clients = {
  clients: [
    {
      id: 's1',
      business_id: 'b1',
      tenant_root_id: 'b1',
      kind: 'analytics',
      name: 'garden.gg',
      publishable_key: 'mfk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA',
      status: 'active',
      require_signature: false,
      has_secret: false,
      created_at: '',
      revoked_at: null,
    },
    {
      id: 'c1',
      business_id: 'b1',
      tenant_root_id: 'b1',
      kind: 'crash',
      name: 'iOS app',
      publishable_key: 'mfk_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB',
      status: 'active',
      require_signature: false,
      has_secret: false,
      created_at: '',
      revoked_at: null,
    },
  ],
};

describe('AnalyticsSitesListComponent', () => {
  let mock: HttpTestingController;
  let fixture: ComponentFixture<AnalyticsSitesListComponent>;

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    mock = TestBed.inject(HttpTestingController);
    fixture = TestBed.createComponent(AnalyticsSitesListComponent);
    fixture.detectChanges();
    mock.expectOne('/api/v1/businesses').flush(biz);
    fixture.detectChanges();
    mock.expectOne('/api/v1/businesses/b1/telemetry/clients').flush(clients);
    fixture.detectChanges();
  });

  afterEach(() => mock.verify());

  it('lists only analytics sites, not crash clients', () => {
    const rows = fixture.nativeElement.querySelectorAll('[data-testid="site-row"]');
    expect(rows.length).toBe(1);
    expect(fixture.nativeElement.textContent).toContain('garden.gg');
    expect(fixture.nativeElement.textContent).not.toContain('iOS app');
  });

  // The embed tag is the deliverable of this screen — if it does not render correctly and in
  // full, the user cannot start collecting anything.
  it('renders a complete, copyable embed tag containing the publishable key', () => {
    const embed = fixture.nativeElement.querySelector('[data-testid="site-embed"]');
    expect(embed).toBeTruthy();
    const tag = embed.textContent as string;
    expect(tag).toContain('<script');
    expect(tag).toContain('/a.js');
    expect(tag).toContain('data-key="mfk_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"');
    expect(tag).toContain('defer');
  });

  // A site created from this screen must never request a signing secret: the mfs_ secret is
  // server-to-server only, and using it here would mean embedding it in a public web page where
  // every visitor could read it.
  it('creates sites with require_signature false', () => {
    const comp = fixture.componentInstance;
    comp.newName = 'example.com';
    comp.create();
    const req = mock.expectOne('/api/v1/businesses/b1/telemetry/clients');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({
      kind: 'analytics',
      name: 'example.com',
      require_signature: false,
    });
    req.flush({ ...clients.clients[0], id: 's2', name: 'example.com' });
    mock.expectOne('/api/v1/businesses/b1/telemetry/clients').flush(clients);
  });

  it('revokes a site', () => {
    const btn = fixture.nativeElement.querySelector('[data-testid="site-revoke"]');
    btn.dispatchEvent(new MouseEvent('click'));
    const req = mock.expectOne('/api/v1/businesses/b1/telemetry/clients/s1/revoke');
    expect(req.request.method).toBe('POST');
    req.flush({ ...clients.clients[0], status: 'revoked' });
    mock.expectOne('/api/v1/businesses/b1/telemetry/clients').flush({
      clients: [{ ...clients.clients[0], status: 'revoked', revoked_at: '2026-07-25T00:00:00Z' }],
    });
    fixture.detectChanges();
    // A revoked site must not still advertise an embed tag — pasting it would collect nothing.
    expect(fixture.nativeElement.querySelector('[data-testid="site-embed"]')).toBeNull();
  });

  it('moves a site through an eligible target and reloads the destination list', () => {
    const move = fixture.nativeElement.querySelector('[data-testid="site-move"]');
    move.dispatchEvent(new MouseEvent('click'));

    const targets = mock.expectOne('/api/v1/businesses/b1/telemetry/clients/s1/move-targets');
    expect(targets.request.method).toBe('GET');
    targets.flush({
      targets: [{ id: 'b2', tenant_root_id: 'b2', name: 'Acme Labs', is_tenant_root: true }],
    });

    const comp = fixture.componentInstance;
    fixture.detectChanges();
    const targetSelect = fixture.nativeElement.querySelector(
      '[data-testid="site-move-target"]',
    ) as HTMLSelectElement;
    expect(targetSelect.textContent).toContain('Acme Labs (master)');
    targetSelect.value = 'b2';
    targetSelect.dispatchEvent(new Event('change'));
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('[data-testid="site-move-confirmation"]').textContent,
    ).toContain('Acme Labs');

    fixture.nativeElement
      .querySelector('[data-testid="site-move-confirm"]')
      .dispatchEvent(new MouseEvent('click'));
    const req = mock.expectOne('/api/v1/businesses/b1/telemetry/clients/s1/move');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({ target_business_id: 'b2' });
    req.flush({ ...clients.clients[0], business_id: 'b2', tenant_root_id: 'b2' });

    expect(comp.businessId()).toBe('b2');
    mock.expectOne('/api/v1/businesses/b2/telemetry/clients').flush({
      clients: [{ ...clients.clients[0], business_id: 'b2', tenant_root_id: 'b2' }],
    });
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('[data-testid="site-name-cell"] a').getAttribute('href'),
    ).toBe('/analytics/b2/s1');
  });

  it('shows an empty state when a business has no sites', () => {
    const comp = fixture.componentInstance;
    comp.selectBusiness('b1');
    mock.expectOne('/api/v1/businesses/b1/telemetry/clients').flush({ clients: [] });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="sites-empty"]')).toBeTruthy();
  });
});
