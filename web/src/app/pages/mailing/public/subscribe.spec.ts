import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { PublicMailingBrand } from '../../../core/mailing.service';
import { MailingSubscribeComponent } from './subscribe';

const BRAND_URL = '/api/v1/mailing/public/mlp_demo/brand';

describe('MailingSubscribeComponent', () => {
  let fixture: ComponentFixture<MailingSubscribeComponent>;
  let component: MailingSubscribeComponent;
  let http: HttpTestingController;

  function mount(brand: PublicMailingBrand | null = null): void {
    fixture = TestBed.createComponent(MailingSubscribeComponent);
    component = fixture.componentInstance;
    fixture.detectChanges();
    http.expectOne(BRAND_URL).flush({ brand });
    fixture.detectChanges();
  }

  beforeEach(() => {
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: {
              paramMap: new Map([['key', 'mlp_demo']]),
              queryParamMap: new Map([['name', 'Product updates']]),
            },
          },
        },
      ],
    });
    http = TestBed.inject(HttpTestingController);
  });

  afterEach(() => http.verify());

  it('shows the shared check-inbox state after a uniform acceptance response', () => {
    mount();
    expect(fixture.nativeElement.textContent).toContain('Join Product updates');
    component.email = 'ada@example.test';
    component.firstName = 'Ada';
    component.submit();
    const request = http.expectOne('/api/v1/mailing/public/mlp_demo/subscribe');
    expect(request.request.body).toEqual({
      email: 'ada@example.test',
      first_name: 'Ada',
      website: '',
    });
    request.flush({ accepted: true });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="mailing-public-done"]')).toBeTruthy();
    expect(fixture.nativeElement.textContent).toContain('Check your inbox');
    expect(fixture.nativeElement.textContent).not.toContain('ada@example.test');
  });

  it('shows retry only for a network failure', () => {
    mount();
    component.email = 'ada@example.test';
    component.submit();
    http.expectOne('/api/v1/mailing/public/mlp_demo/subscribe').error(new ProgressEvent('network'));
    fixture.detectChanges();
    expect(
      fixture.nativeElement.querySelector('[data-testid="mailing-public-error"]'),
    ).toBeTruthy();
    expect(component.done()).toBe(false);
  });

  it('collapses HTTP failures to the same check-inbox state', () => {
    mount();
    component.email = 'ada@example.test';
    component.submit();
    http
      .expectOne('/api/v1/mailing/public/mlp_demo/subscribe')
      .flush({ error: 'invalid request' }, { status: 400, statusText: 'Bad Request' });
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelector('[data-testid="mailing-public-done"]')).toBeTruthy();
  });

  it('keeps the generic header when the list has no brand', () => {
    mount();
    const element: HTMLElement = fixture.nativeElement;
    expect(element.querySelector('[data-testid="mailing-public-brand-name"]')?.textContent).toBe(
      'Mailing',
    );
    expect(element.querySelector('[data-testid="mailing-public-brand-logo"]')).toBeNull();
  });

  it('shows the brand logo and accent color in the shell header', () => {
    mount({
      name: 'Acme',
      logo_url: 'https://mail.example.test/m/b/brand-id/logo?v=abcd',
      colors: {
        background: '#f4f6f8',
        surface: '#ffffff',
        text: '#17212b',
        accent: '#ff0000',
        header_background: '#ffffff',
        header_text: '#17212b',
      },
    });
    const element: HTMLElement = fixture.nativeElement;
    const logo = element.querySelector<HTMLImageElement>(
      '[data-testid="mailing-public-brand-logo"]',
    );
    expect(logo?.getAttribute('src')).toBe('https://mail.example.test/m/b/brand-id/logo?v=abcd');
    expect(logo?.alt).toBe('Acme');
    expect(element.querySelector('[data-testid="mailing-public-brand-name"]')).toBeNull();
    const bar = element.querySelector<HTMLElement>('.public-bar');
    expect(['#ff0000', 'rgb(255, 0, 0)']).toContain(bar?.style.borderBottomColor);
    expect(element.textContent).toContain('Powered by');
  });

  it('falls back to the brand name when there is no logo', () => {
    mount({
      name: 'Acme',
      logo_url: null,
      colors: {
        background: '#f4f6f8',
        surface: '#ffffff',
        text: '#17212b',
        accent: '#ff0000',
        header_background: '#ffffff',
        header_text: '#17212b',
      },
    });
    const element: HTMLElement = fixture.nativeElement;
    const name = element.querySelector<HTMLElement>('[data-testid="mailing-public-brand-name"]');
    expect(name?.textContent).toBe('Acme');
    expect(['#ff0000', 'rgb(255, 0, 0)']).toContain(name?.style.color);
  });
});
