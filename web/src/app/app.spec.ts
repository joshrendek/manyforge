import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { App } from './app';

describe('App shell', () => {
  let mock: HttpTestingController;

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])],
    });
    mock = TestBed.inject(HttpTestingController);
  });
  afterEach(() => {
    localStorage.clear();
  });

  it('renders the persistent sidebar with the Support nav when authenticated', () => {
    localStorage.setItem('mf_access', 'tok');
    const fixture = TestBed.createComponent(App);
    fixture.detectChanges(); // ngOnInit fires me()
    mock.expectOne('/api/v1/me').flush({
      id: 'u1',
      email: 'a@b.test',
      display_name: 'Ada',
      email_verified: true,
      status: 'active',
    });
    fixture.detectChanges();
    const el: HTMLElement = fixture.nativeElement;
    expect(el.querySelector('[data-testid="app-sidebar"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="nav-support"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="nav-dashboard"]')).toBeTruthy();
    expect(el.textContent).toContain('Ada');
  });

  it('renders the bare topbar (no sidebar) when unauthenticated', () => {
    const fixture = TestBed.createComponent(App);
    fixture.detectChanges();
    const el: HTMLElement = fixture.nativeElement;
    expect(el.querySelector('[data-testid="app-sidebar"]')).toBeNull();
    expect(el.querySelector('.topbar')).toBeTruthy();
    mock.expectNone('/api/v1/me');
  });

  it('renders all nav items including Accounting', () => {
    localStorage.setItem('mf_access', 'tok');
    const f = TestBed.createComponent(App); f.detectChanges();
    mock.expectOne('/api/v1/me').flush({ id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' });
    f.detectChanges();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('[data-testid="nav-dashboard"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="nav-support"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="nav-accounting"]')).toBeTruthy();
    expect(el.querySelector('[data-testid="theme-toggle"]')).toBeTruthy();
  });

  it('shows a pending-count badge on the Approvals nav for the current business', () => {
    localStorage.setItem('mf_access', 'tok');
    localStorage.setItem('mf-current-business', 'b1');
    const f = TestBed.createComponent(App);
    f.detectChanges();
    mock.expectOne('/api/v1/me').flush({ id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' });
    mock.expectOne('/api/v1/businesses/b1/approvals').flush({ items: [{ id: 'x1' }, { id: 'x2' }] });
    f.detectChanges();
    const badge = f.nativeElement.querySelector('[data-testid="nav-approvals"] .nav-badge');
    expect(badge?.textContent).toContain('2');
  });
});
