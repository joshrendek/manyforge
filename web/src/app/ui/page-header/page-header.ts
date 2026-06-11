import { Component, Input } from '@angular/core';

@Component({
  selector: 'mf-page-header',
  standalone: true,
  template: `
    <header class="mf-pageheader">
      <div>
        <h1>{{ title }}</h1>
        @if (subtitle) { <div class="mf-pageheader-sub">{{ subtitle }}</div> }
      </div>
      <div class="mf-pageheader-actions"><ng-content select="[actions]" /></div>
    </header>`,
  styles: [`
    .mf-pageheader{display:flex;justify-content:space-between;align-items:flex-start;gap:16px;margin-bottom:20px}
    h1{font-size:var(--mf-fs-2xl);font-weight:680;letter-spacing:-.025em;margin:0}
    .mf-pageheader-sub{color:var(--mf-text-muted);font-size:var(--mf-fs-sm);margin-top:3px}
    .mf-pageheader-actions{display:flex;gap:8px;align-items:center}
  `],
})
export class PageHeader {
  @Input() title = '';
  @Input() subtitle = '';
}
