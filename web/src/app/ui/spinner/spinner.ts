import { Component } from '@angular/core';

@Component({
  selector: 'mf-spinner',
  standalone: true,
  template: `<span class="mf-spinner" role="status" aria-busy="true" aria-label="Loading"></span>`,
  styles: [`
    .mf-spinner{display:inline-block;width:16px;height:16px;border:2px solid var(--mf-border-strong);border-top-color:var(--mf-accent);border-radius:50%;animation:mf-spin .6s linear infinite}
    @keyframes mf-spin{to{transform:rotate(360deg)}}
    @media (prefers-reduced-motion: reduce){.mf-spinner{animation-duration:1.6s}}
  `],
})
export class Spinner {}
