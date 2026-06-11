import { Component, Input } from '@angular/core';
import { Tone } from '../status';

@Component({
  selector: 'mf-status-pill',
  standalone: true,
  template: `<span class="mf-pill mf-pill-{{ tone }}"><span class="mf-dot"></span>{{ label }}</span>`,
})
export class StatusPill {
  @Input() tone: Tone = 'neutral';
  @Input() label = '';
}
