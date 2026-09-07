import { Component, Input } from '@angular/core';

@Component({
  selector: 'mf-empty-state',
  standalone: true,
  template: `
    <div class="mf-empty">
      @if (icon) { <div class="mf-empty-ico">{{ icon }}</div> }
      <div>
        <b>{{ title }}</b>
        <div class="mf-empty-body"><ng-content /></div>
      </div>
      <ng-content select="[action]" />
    </div>`,
})
export class EmptyState {
  @Input() icon = '';
  @Input() title = '';
}
