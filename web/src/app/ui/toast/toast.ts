import { Component, inject } from '@angular/core';
import { ToastService } from './toast.service';

@Component({
  selector: 'mf-toast-host',
  standalone: true,
  template: `
    <div class="mf-toast-stack" aria-live="polite">
      @for (t of toasts.toasts(); track t.id) {
        <div class="mf-toast" [class.mf-toast-err]="t.kind === 'error'" data-testid="toast">
          <span>{{ t.kind === 'error' ? '⚠' : '✓' }}</span><span>{{ t.message }}</span>
          <button class="mf-toast-x" (click)="toasts.dismiss(t.id)" aria-label="Dismiss">×</button>
        </div>
      }
    </div>`,
})
export class ToastHost {
  readonly toasts = inject(ToastService);
}
