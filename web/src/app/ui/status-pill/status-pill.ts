import { Component, Input } from '@angular/core';
import { Tone } from '../status';

@Component({
  selector: 'mf-status-pill',
  standalone: true,
  // The colored dot is decorative (aria-hidden) so a screen reader announces only the text.
  // ariaLabel overrides the accessible name when the visible label lacks context on its own
  // (e.g. a bare finding count "3" → "3 findings"); role=img groups dot+text as one label.
  template: `<span class="mf-pill mf-pill-{{ tone }}" role="img" [attr.aria-label]="ariaLabel || label"
    ><span class="mf-dot" aria-hidden="true"></span><span aria-hidden="true">{{ label }}</span></span>`,
})
export class StatusPill {
  @Input() tone: Tone = 'neutral';
  @Input() label = '';
  @Input() ariaLabel = '';
}
