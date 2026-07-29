import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { ComponentFixture } from '@angular/core/testing';
import { ActivatedRoute, Router, convertToParamMap, provideRouter } from '@angular/router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TenantMergeOperation, TenantMergeSourceOptions } from '../core/tenant-merge.service';
import { TenantMergeComponent } from './tenant-merge';

const source = {
  id: 'source',
  parent_id: null,
  tenant_root_id: 'source',
  name: 'Source Holdings',
  status: 'active',
  is_tenant_root: true,
};

const destination = {
  id: 'division',
  parent_id: 'destination-root',
  tenant_root_id: 'destination-root',
  name: 'Platform Division',
  status: 'active',
  is_tenant_root: false,
};

const options: TenantMergeSourceOptions = {
  source_root_id: source.id,
  source_root_name: source.name,
  destinations: [
    {
      id: 'destination-root',
      name: 'Destination Group',
      tenant_root_id: 'destination-root',
      tenant_root_name: 'Destination Group',
      hierarchy_path: 'Destination Group',
      is_tenant_root: true,
    },
    {
      id: destination.id,
      name: destination.name,
      tenant_root_id: 'destination-root',
      tenant_root_name: 'Destination Group',
      hierarchy_path: 'Destination Group / Platform Division',
      is_tenant_root: false,
    },
  ],
};

function merge(
  status: TenantMergeOperation['status'],
  overrides: Partial<TenantMergeOperation> = {},
): TenantMergeOperation {
  return {
    id: 'operation-1',
    correlation_id: 'correlation-1',
    source_root_id: source.id,
    destination_parent_id: destination.id,
    destination_root_id: 'destination-root',
    status,
    preflight_generation: status === 'preflight_required' ? null : 'generation-1',
    module_counts: {
      crm: { rows: 12, bytes: 2048 },
      support: { rows: 4, bytes: 1024 },
    },
    conflicts: [],
    warnings: [],
    affected_rows: 16,
    estimated_bytes: 3072,
    source_businesses: 3,
    resulting_depth: 4,
    attachment_count: 0,
    attachment_bytes: 0,
    preflight_completed_at: '2026-07-29T00:00:00Z',
    ready_at: status === 'ready' ? '2026-07-29T00:00:00Z' : null,
    created_at: '2026-07-29T00:00:00Z',
    updated_at: '2026-07-29T00:00:00Z',
    failure: null,
    ...overrides,
  };
}

describe('TenantMergeComponent', () => {
  afterEach(() => {
    sessionStorage.clear();
  });

  function mount(params: { sourceRootId?: string; operationId?: string }) {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useValue: { snapshot: { paramMap: convertToParamMap(params) } },
        },
      ],
    });
    const fixture = TestBed.createComponent(TenantMergeComponent);
    fixture.detectChanges();
    return {
      fixture,
      ctrl: TestBed.inject(HttpTestingController),
      router: TestBed.inject(Router),
    };
  }

  function flushLabels(ctrl: HttpTestingController, includePreview = true) {
    ctrl.expectOne('/api/v1/businesses/source').flush(source);
    ctrl.expectOne('/api/v1/businesses/division').flush(destination);
    ctrl.expectOne('/api/v1/tenant-merge-options').flush({ sources: [options] });
    if (includePreview) {
      ctrl.expectOne('/api/v1/businesses').flush({
        items: [
          source,
          {
            id: 'source-child',
            parent_id: source.id,
            tenant_root_id: source.id,
            name: 'Source Child',
            status: 'active',
            is_tenant_root: false,
          },
        ],
      });
    }
  }

  function setInput(
    fixture: ComponentFixture<TenantMergeComponent>,
    testId: string,
    value: string,
  ) {
    const input: HTMLInputElement = fixture.nativeElement.querySelector(
      `[data-testid="${testId}"]`,
    );
    input.value = value;
    input.dispatchEvent(new Event('input'));
    fixture.detectChanges();
  }

  it('searches server-authorized destinations and creates a stable operation', () => {
    const { fixture, ctrl, router } = mount({ sourceRootId: source.id });
    ctrl.expectOne('/api/v1/tenant-merge-options').flush({ sources: [options] });
    fixture.detectChanges();

    expect(
      fixture.nativeElement.querySelectorAll('[data-testid="destination-option"]'),
    ).toHaveLength(2);
    setInput(fixture, 'destination-search', 'platform');
    expect(
      fixture.nativeElement.querySelectorAll('[data-testid="destination-option"]'),
    ).toHaveLength(1);

    const navigate = vi.spyOn(router, 'navigateByUrl').mockResolvedValue(true);
    (
      fixture.nativeElement.querySelector('[data-testid="destination-option"]') as HTMLButtonElement
    ).click();
    fixture.detectChanges();
    (
      fixture.nativeElement.querySelector('[data-testid="review-merge"]') as HTMLButtonElement
    ).click();

    const request = ctrl.expectOne('/api/v1/businesses/source/tenant-merges');
    expect(request.request.method).toBe('POST');
    expect(request.request.body).toEqual({ destination_parent_id: 'division' });
    expect(request.request.headers.get('Idempotency-Key')).toMatch(/^dashboard-/);
    request.flush(merge('ready'));
    expect(navigate).toHaveBeenCalledWith('/tenant-merges/operation-1');
  });

  it('keeps blocked and stale operations disabled until a current clean preflight', () => {
    const { fixture, ctrl } = mount({ operationId: 'operation-1' });
    ctrl.expectOne('/api/v1/tenant-merges/operation-1').flush(
      merge('preflight_required', {
        conflicts: [
          {
            code: 'company_domain_collision',
            module: 'crm',
            object: 'company',
            count: 2,
          },
        ],
      }),
    );
    flushLabels(ctrl);
    fixture.detectChanges();

    expect(
      fixture.nativeElement.querySelector('[data-testid="merge-blockers"]')?.textContent,
    ).toContain('Company Domain Collision');
    expect(
      fixture.nativeElement.querySelector('[data-testid="resulting-tree-preview"]')?.textContent,
    ).toContain('Source Child');
    expect(
      (fixture.nativeElement.querySelector('[data-testid="start-merge"]') as HTMLButtonElement)
        .disabled,
    ).toBe(true);

    (
      fixture.nativeElement.querySelector('[data-testid="rerun-preflight"]') as HTMLButtonElement
    ).click();
    ctrl.expectOne('/api/v1/tenant-merges/operation-1/preflight').flush(merge('ready'));
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="merge-blockers"]')).toBeFalsy();
  });

  it('requires exact names and fresh authentication, then renders the reloaded hierarchy', () => {
    const { fixture, ctrl } = mount({ operationId: 'operation-1' });
    ctrl.expectOne('/api/v1/tenant-merges/operation-1').flush(merge('ready'));
    flushLabels(ctrl);
    fixture.detectChanges();

    const start = () =>
      fixture.nativeElement.querySelector('[data-testid="start-merge"]') as HTMLButtonElement;
    expect(start().disabled).toBe(true);
    setInput(fixture, 'confirm-source', source.name);
    setInput(fixture, 'confirm-destination', destination.name);
    setInput(fixture, 'confirm-password', 'wrong-password');
    expect(start().disabled).toBe(false);
    start().click();

    const rejected = ctrl.expectOne('/api/v1/tenant-merges/operation-1/confirm');
    expect(rejected.request.body).toEqual({
      source_name: source.name,
      destination_name: destination.name,
      password: 'wrong-password',
    });
    rejected.flush(
      { code: 'REAUTHENTICATION_FAILED', message: 'reauthentication required' },
      { status: 401, statusText: 'Unauthorized' },
    );
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('[data-testid="confirmation-error"]')?.textContent,
    ).toContain('Fresh authentication failed');

    setInput(fixture, 'confirm-password', 'current-password');
    start().click();
    ctrl.expectOne('/api/v1/tenant-merges/operation-1/confirm').flush(merge('succeeded'));
    ctrl.expectOne('/api/v1/businesses').flush({
      items: [
        {
          id: 'destination-root',
          parent_id: null,
          tenant_root_id: 'destination-root',
          name: 'Destination Group',
          status: 'active',
          is_tenant_root: true,
        },
        {
          ...destination,
        },
        {
          ...source,
          parent_id: destination.id,
          tenant_root_id: 'destination-root',
          is_tenant_root: false,
        },
      ],
    });
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('[data-testid="merge-success"]')?.textContent,
    ).toContain('Master moved successfully');
    expect(
      fixture.nativeElement.querySelector('[data-testid="result-hierarchy"]')?.textContent,
    ).toContain(source.name);
  });

  it('resumes running status and distinguishes operator intervention from safe rollback', () => {
    const running = mount({ operationId: 'operation-1' });
    running.ctrl.expectOne('/api/v1/tenant-merges/operation-1').flush(merge('running'));
    flushLabels(running.ctrl, false);
    running.fixture.detectChanges();
    expect(
      running.fixture.nativeElement.querySelector('[data-testid="merge-running"]')?.textContent,
    ).toContain('operator intervention is required');
    running.fixture.destroy();
    TestBed.resetTestingModule();

    const failed = mount({ operationId: 'operation-1' });
    failed.ctrl.expectOne('/api/v1/tenant-merges/operation-1').flush(
      merge('failed', {
        failure: {
          code: 'CUTOVER_FAILED',
          stage: 'rewrite_catalog',
          operator_correlation_id: 'correlation-1',
        },
      }),
    );
    flushLabels(failed.ctrl, false);
    failed.fixture.detectChanges();
    const failureText =
      failed.fixture.nativeElement.querySelector('[data-testid="merge-failed"]')?.textContent ?? '';
    expect(failureText).toContain('rolled back safely');
    expect(failureText).toContain('Do not repeat');
    expect(
      failed.fixture.nativeElement.querySelector('[data-testid="rerun-preflight"]'),
    ).toBeFalsy();
  });
});
