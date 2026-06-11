import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ThemeToggle } from './theme-toggle';
import { ThemeService } from '../../core/theme.service';

describe('mf-theme-toggle', () => {
  beforeEach(() => { localStorage.clear(); });
  afterEach(() => { localStorage.clear(); document.documentElement.setAttribute('data-theme', 'light'); });

  it('toggles the theme on click', () => {
    const f = TestBed.createComponent(ThemeToggle);
    const theme = TestBed.inject(ThemeService); f.detectChanges();
    const btn: HTMLButtonElement = f.nativeElement.querySelector('[data-testid="theme-toggle"]');
    const before = theme.theme();
    btn.click(); f.detectChanges();
    expect(theme.theme()).not.toBe(before);
  });
});
