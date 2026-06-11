import { Component, inject } from '@angular/core';
import { ThemeService } from '../../core/theme.service';

@Component({
  selector: 'mf-theme-toggle',
  standalone: true,
  template: `
    <button class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="theme-toggle"
      [attr.aria-label]="theme.theme() === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'"
      (click)="theme.toggle()">{{ theme.theme() === 'dark' ? '☀' : '☾' }}</button>`,
})
export class ThemeToggle {
  readonly theme = inject(ThemeService);
}
