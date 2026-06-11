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
  styles: [`
    .mf-empty{display:flex;flex-direction:column;align-items:center;gap:10px;padding:34px;color:var(--mf-text-muted);text-align:center}
    .mf-empty-ico{width:42px;height:42px;border-radius:12px;background:var(--mf-accent-soft);display:flex;align-items:center;justify-content:center;font-size:20px;color:var(--mf-accent-text)}
    .mf-empty-body{font-size:var(--mf-fs-sm);color:var(--mf-text-muted)}
  `],
})
export class EmptyState {
  @Input() icon = '';
  @Input() title = '';
}
