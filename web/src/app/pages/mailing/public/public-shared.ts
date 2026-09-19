import { Component, Input } from '@angular/core';
import { ThemeToggle } from '../../../ui/theme-toggle/theme-toggle';

@Component({
  selector: 'app-mailing-public-shell',
  standalone: true,
  imports: [ThemeToggle],
  template: `
    <div class="public-shell" data-testid="mailing-public-shell">
      <header class="public-bar" [style.border-bottom-color]="accentColor || null">
        @if (brandLogoUrl) {
          <img
            class="brand-logo"
            data-testid="mailing-public-brand-logo"
            [src]="brandLogoUrl"
            [alt]="brandName || 'Mailing'"
          />
        } @else {
          <span
            class="brand"
            data-testid="mailing-public-brand-name"
            [style.color]="accentColor || null"
            >{{ brandName || 'Mailing' }}</span
          >
        }
        <span class="spacer"></span>
        <mf-theme-toggle />
      </header>
      <main class="public-body"><ng-content /></main>
      <footer class="public-foot">
        <span class="mf-hint">Powered by <b>ManyForge</b></span>
      </footer>
    </div>
  `,
  styles: [
    `
      .public-shell {
        min-height: 100vh;
        display: flex;
        flex-direction: column;
        background: var(--mf-bg, var(--mf-surface));
      }
      .public-bar {
        display: flex;
        align-items: center;
        gap: 12px;
        padding: 14px 20px;
        border-bottom: 1px solid var(--mf-border);
      }
      .brand {
        font-weight: 680;
        letter-spacing: -0.01em;
      }
      .brand-logo {
        display: block;
        max-height: 40px;
        max-width: 200px;
        width: auto;
        height: auto;
      }
      .spacer {
        flex: 1;
      }
      .public-body {
        width: 100%;
        max-width: 560px;
        margin: 0 auto;
        padding: 48px 20px;
        flex: 1;
      }
      .public-foot {
        text-align: center;
        padding: 20px;
        border-top: 1px solid var(--mf-border);
      }
    `,
  ],
})
export class MailingPublicShellComponent {
  @Input() brandName: string | null = null;
  @Input() brandLogoUrl: string | null = null;
  @Input() accentColor: string | null = null;
}

@Component({
  selector: 'app-mailing-public-done',
  standalone: true,
  template: `
    <section class="mf-card done" data-testid="mailing-public-done" role="status">
      <h1>{{ heading }}</h1>
      <p class="mf-hint">{{ message }}</p>
    </section>
  `,
  styles: [
    `
      .done {
        text-align: center;
        padding: 32px;
      }
      h1 {
        margin: 0 0 8px;
        font-size: var(--mf-fs-xl);
      }
      p {
        margin: 0;
      }
    `,
  ],
})
export class MailingPublicDoneComponent {
  @Input() heading = 'All set';
  @Input() message = 'Your request has been processed.';
}
