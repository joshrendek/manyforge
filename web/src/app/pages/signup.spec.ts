import { provideHttpClient } from '@angular/common/http';
import { provideHttpClientTesting } from '@angular/common/http/testing';
import { TestBed } from '@angular/core/testing';
import { provideRouter } from '@angular/router';
import { afterEach, describe, expect, it } from 'vitest';
import { SignupComponent } from './signup';

describe('SignupComponent', () => {
  afterEach(() => document.documentElement.setAttribute('data-theme', 'light'));
  function mount() {
    TestBed.configureTestingModule({ providers: [provideHttpClient(), provideHttpClientTesting(), provideRouter([])] });
    const f = TestBed.createComponent(SignupComponent); f.detectChanges(); return f;
  }
  it('renders styled inputs and submit', () => {
    const el: HTMLElement = mount().nativeElement;
    expect(el.querySelector('input.mf-input[data-testid="signup-email"]')).toBeTruthy();
    expect(el.querySelector('button.mf-btn-primary[data-testid="signup-submit"]')).toBeTruthy();
  });
  it('renders in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    expect(mount().nativeElement.querySelector('.mf-card')).toBeTruthy();
  });
});
