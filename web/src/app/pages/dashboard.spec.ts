import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, describe, expect, it } from 'vitest';
import { DashboardComponent } from './dashboard';

describe('DashboardComponent', () => {
  afterEach(() => {
    document.documentElement.setAttribute('data-theme', 'light');
    localStorage.clear();
  });

  function mount(
    mergeSources: {
      source_root_id: string;
      source_root_name: string;
      destinations: unknown[];
    }[] = [],
  ) {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    const f = TestBed.createComponent(DashboardComponent);
    f.componentInstance.view.set('ledger');
    f.detectChanges();

    const ctrl = TestBed.inject(HttpTestingController);
    ctrl.expectOne('/api/v1/me').flush({
      id: '1',
      email: 'a@b.c',
      display_name: 'A',
      email_verified: true,
      status: 'active',
    });
    ctrl.expectOne('/api/v1/businesses').flush({
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
    });
    ctrl.expectOne('/api/v1/tenant-merge-options').flush({
      sources: mergeSources,
    });
    f.detectChanges();
    return { f, ctrl };
  }


  it('shows Move master only when the authorization read returns an eligible source', () => {
    const unauthorized = mount();
    expect(
      unauthorized.f.nativeElement.querySelector('[data-testid="move-master"]'),
    ).toBeFalsy();

    TestBed.resetTestingModule();
    const authorized = mount([
      {
        source_root_id: 'b1',
        source_root_name: 'Acme',
        destinations: [{ id: 'd1' }],
      },
    ]);
    const action: HTMLAnchorElement | null =
      authorized.f.nativeElement.querySelector('[data-testid="move-master"]');
    expect(action).toBeTruthy();
    expect(action?.getAttribute('href')).toBe('/tenant-merges/new/b1');
  });
});
