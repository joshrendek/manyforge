import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ThemeService } from './theme.service';

describe('ThemeService', () => {
  beforeEach(() => { localStorage.clear(); document.documentElement.removeAttribute('data-theme'); });
  afterEach(() => { localStorage.clear(); document.documentElement.setAttribute('data-theme', 'light'); });

  it('defaults to light when nothing saved and system is not dark', () => {
    const svc = TestBed.inject(ThemeService);
    TestBed.flushEffects();
    expect(svc.theme()).toBe('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });

  it('reads a saved theme from localStorage', () => {
    localStorage.setItem('mf-theme', 'dark');
    const svc = TestBed.inject(ThemeService);
    TestBed.flushEffects();
    expect(svc.theme()).toBe('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
  });

  it('toggle() flips the theme and persists it', () => {
    const svc = TestBed.inject(ThemeService);
    svc.toggle();
    TestBed.flushEffects();
    expect(svc.theme()).toBe('dark');
    expect(localStorage.getItem('mf-theme')).toBe('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
  });
});
