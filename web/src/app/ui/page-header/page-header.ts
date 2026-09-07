import { Component, Input, inject } from '@angular/core';
import { CurrentBusinessService } from '../../core/current-business.service';

@Component({
  selector: 'mf-page-header',
  standalone: true,
  template: `
    <header class="mf-pageheader">
      <div>
        @if (eyebrow ?? current.businessName(); as context) {
          <div class="mf-eyebrow">{{ context }}</div>
        }
        <h1>{{ title }}</h1>
        @if (subtitle) { <div class="mf-pageheader-sub">{{ subtitle }}</div> }
      </div>
      <div class="mf-pageheader-actions"><ng-content select="[actions]" /></div>
    </header>`,
})
export class PageHeader {
  readonly current = inject(CurrentBusinessService);
  @Input() eyebrow: string | undefined;
  @Input() title = '';
  @Input() subtitle = '';
}
