import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ToastService } from '../../ui/toast/toast.service';
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
      allowed_origins: ['https://garden.gg'],
      status: 'active',
      require_signature: false,
      has_secret: false,
      created_at: '',
      revoked_at: null,
      analytics_health: {
        status: 'never_seen',
        receiving_data: false,
        last_accepted_at: null,
        activity_data_as_of: '2026-07-25T00:00:00Z',
        data_as_of: '2026-07-25T00:00:00Z',
      },
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
    expect(fixture.nativeElement.textContent).toContain('https://garden.gg');
  });

  it('replaces allowed origins without rotating or losing site health', () => {
    fixture.nativeElement
      .querySelector('[data-testid="site-manage-origins"]')
      .dispatchEvent(new MouseEvent('click'));
    fixture.detectChanges();
    const comp = fixture.componentInstance;
    expect(comp.originsDraft).toBe('https://garden.gg');
    comp.originsDraft = 'https://www.garden.gg\nhttp://localhost:4300';
    comp.saveOrigins(comp.sites()[0]);

    const req = mock.expectOne('/api/v1/businesses/b1/telemetry/clients/s1/allowed-origins');
    expect(req.request.method).toBe('PUT');
    expect(req.request.body).toEqual({
      allowed_origins: ['https://www.garden.gg', 'http://localhost:4300'],
    });
    req.flush({
      ...clients.clients[0],
      allowed_origins: ['https://www.garden.gg', 'http://localhost:4300'],
      analytics_health: undefined,
    });
    fixture.detectChanges();

    expect(fixture.nativeElement.textContent).toContain('https://www.garden.gg');
    expect(
      fixture.nativeElement.querySelector('[data-testid="site-health-message"]').textContent,
    ).toContain('No accepted event yet');
    expect(TestBed.inject(ToastService).toasts().at(-1)?.message).toContain(
      'embed key is unchanged',
    );
  });

  it('labels legacy unrestricted sites and lets operators restrict them', () => {
    fixture.componentInstance.selectBusiness('b1');
    mock.expectOne('/api/v1/businesses/b1/telemetry/clients').flush({
      clients: [{ ...clients.clients[0], allowed_origins: [] }],
    });
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('[data-testid="site-origin-unrestricted"]').textContent,
    ).toContain('any origin');
    expect(fixture.nativeElement.querySelector('[data-testid="site-manage-origins"]')).toBeTruthy();
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

  it('shows a setup checklist when the collector has never accepted an event', () => {
    expect(
      fixture.nativeElement.querySelector('[data-testid="site-status-cell"]').textContent,
    ).toContain('Never seen');
    const checklist = fixture.nativeElement.querySelector('[data-testid="site-install-checklist"]');
    expect(checklist.textContent).toContain('Paste it into the site');
    expect(checklist.textContent).toContain('Visit or reload the site');
    expect(
      fixture.nativeElement.querySelector('[data-testid="site-health-message"]').textContent,
    ).toContain('No accepted event yet');
    expect(
      fixture.nativeElement
        .querySelector('[data-testid="site-check-installation"]')
        .getAttribute('aria-label'),
    ).toContain('garden.gg');
    expect(checklist.closest('[data-testid="site-health"]').textContent).toContain(
      'Installation health for garden.gg',
    );
  });

  it('checks installation, reflects recovery, and preserves keyboard focus', async () => {
    const checkButton = fixture.nativeElement.querySelector(
      '[data-testid="site-check-installation"]',
    ) as HTMLButtonElement;
    checkButton.focus();
    checkButton.dispatchEvent(new MouseEvent('click'));

    mock.expectOne('/api/v1/businesses/b1/telemetry/clients').flush({
      clients: [
        {
          ...clients.clients[0],
          analytics_health: {
            status: 'healthy',
            receiving_data: true,
            last_accepted_at: '2026-07-25T00:01:00Z',
            activity_data_as_of: '2026-07-25T00:01:30Z',
            data_as_of: '2026-07-25T00:01:30Z',
          },
        },
      ],
    });
    fixture.detectChanges();
    await new Promise((resolve) => setTimeout(resolve));
    fixture.detectChanges();

    const statusCell = fixture.nativeElement.querySelector(
      '[data-testid="site-status-cell"]',
    ) as HTMLElement;
    expect(statusCell.textContent).toContain('Healthy');
    expect(document.activeElement).toBe(statusCell);
    expect(
      fixture.nativeElement.querySelector('[data-testid="site-health-message"]').textContent,
    ).toContain('Data is arriving');
    expect(
      fixture.nativeElement.querySelector('[data-testid="site-install-checklist"]'),
    ).toBeNull();
    expect(TestBed.inject(ToastService).toasts().at(-1)?.message).toContain(
      'Installation verified',
    );
  });

  it('keeps the health action available while processing catches up', () => {
    fixture.nativeElement
      .querySelector('[data-testid="site-check-installation"]')
      .dispatchEvent(new MouseEvent('click'));
    mock.expectOne('/api/v1/businesses/b1/telemetry/clients').flush({
      clients: [
        {
          ...clients.clients[0],
          analytics_health: {
            ...clients.clients[0].analytics_health,
            status: 'checking',
            activity_data_as_of: null,
          },
        },
      ],
    });
    fixture.detectChanges();

    expect(
      fixture.nativeElement.querySelector('[data-testid="site-health-message"]').textContent,
    ).toContain('still processing');
    expect(
      fixture.nativeElement.querySelector('[data-testid="site-check-installation"]'),
    ).toBeTruthy();
    expect(TestBed.inject(ToastService).toasts().at(-1)?.message).toContain(
      'processing is catching up',
    );
  });

  it('recovers from an installation-check request failure', () => {
    fixture.nativeElement
      .querySelector('[data-testid="site-check-installation"]')
      .dispatchEvent(new MouseEvent('click'));
    mock
      .expectOne('/api/v1/businesses/b1/telemetry/clients')
      .flush({ error: 'unavailable' }, { status: 503, statusText: 'Unavailable' });
    fixture.detectChanges();

    expect(fixture.componentInstance.verifyingSiteId()).toBe('');
    expect(TestBed.inject(ToastService).toasts().at(-1)?.message).toContain(
      'Could not check installation',
    );
  });

  it('shows a stale site with the exact last accepted event', () => {
    fixture.componentInstance.selectBusiness('b1');
    mock.expectOne('/api/v1/businesses/b1/telemetry/clients').flush({
      clients: [
        {
          ...clients.clients[0],
          analytics_health: {
            ...clients.clients[0].analytics_health,
            status: 'stale',
            last_accepted_at: '2026-07-20T12:00:00Z',
          },
        },
      ],
    });
    fixture.detectChanges();

    expect(
      fixture.nativeElement.querySelector('[data-testid="site-status-cell"]').textContent,
    ).toContain('Stale');
    expect(
      fixture.nativeElement.querySelector('[data-testid="site-health"]').textContent,
    ).toContain('2026-07-20T12:00:00Z');

    fixture.nativeElement
      .querySelector('[data-testid="site-check-installation"]')
      .dispatchEvent(new MouseEvent('click'));
    mock.expectOne('/api/v1/businesses/b1/telemetry/clients').flush({
      clients: [
        {
          ...clients.clients[0],
          analytics_health: {
            ...clients.clients[0].analytics_health,
            status: 'stale',
            last_accepted_at: '2026-07-20T12:00:00Z',
          },
        },
      ],
    });
    expect(TestBed.inject(ToastService).toasts().at(-1)?.message).toContain('No recent event yet');
  });

  // A site created from this screen must never request a signing secret: the mfs_ secret is
  // server-to-server only, and using it here would mean embedding it in a public web page where
  // every visitor could read it.
  it('creates sites with require_signature false', () => {
    const comp = fixture.componentInstance;
    comp.newName = 'example.com';
    comp.newOrigin = 'https://example.com';
    comp.create();
    const req = mock.expectOne('/api/v1/businesses/b1/telemetry/clients');
    expect(req.request.method).toBe('POST');
    expect(req.request.body).toEqual({
      kind: 'analytics',
      name: 'example.com',
      require_signature: false,
      allowed_origins: ['https://example.com'],
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
