import { Component } from '@angular/core';

@Component({
  selector: 'mf-spinner',
  standalone: true,
  template: `<span class="mf-spinner" role="status" aria-busy="true" aria-label="Loading"></span>`,
})
export class Spinner {}
