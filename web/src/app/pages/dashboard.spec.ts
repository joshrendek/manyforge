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

  function mount() {
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    const f = TestBed.createComponent(DashboardComponent);
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
    f.detectChanges();
    return { f, ctrl };
  }

  it('renders page header, biz row, create input, and accounting link', () => {
    const { f } = mount();
    const el: HTMLElement = f.nativeElement;

    expect(el.querySelector('mf-page-header')).toBeTruthy();
    const h1 = el.querySelector('mf-page-header h1');
    expect(h1?.textContent).toContain('Your businesses');

    expect(el.querySelector('[data-testid="biz-row"]')).toBeTruthy();

    expect(el.querySelector('input.mf-input')).toBeTruthy();

    expect(el.querySelector('[data-testid="nav-accounting"]')).toBeTruthy();
  });

  it('renders .mf-card in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    const { f } = mount();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('.mf-card')).toBeTruthy();
  });
});
