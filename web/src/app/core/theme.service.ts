import { Injectable, effect, signal } from '@angular/core';

export type Theme = 'light' | 'dark';
const KEY = 'mf-theme';

@Injectable({ providedIn: 'root' })
export class ThemeService {
  readonly theme = signal<Theme>(this.initial());

  constructor() {
    effect(() => {
      const t = this.theme();
      document.documentElement.setAttribute('data-theme', t);
      try { localStorage.setItem(KEY, t); } catch { /* ignore */ }
    });
  }

  private initial(): Theme {
    let saved: string | null = null;
    try { saved = localStorage.getItem(KEY); } catch { /* ignore */ }
    if (saved === 'light' || saved === 'dark') return saved;
    return 'dark';
  }

  toggle(): void { this.theme.set(this.theme() === 'dark' ? 'light' : 'dark'); }
  setTheme(t: Theme): void { this.theme.set(t); }
}
