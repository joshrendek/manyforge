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
  styles: [`
    .mf-toast-stack{position:fixed;right:20px;bottom:20px;display:flex;flex-direction:column;gap:10px;z-index:50}
    .mf-toast{display:flex;align-items:center;gap:10px;background:var(--mf-surface);border:1px solid var(--mf-border);border-left:3px solid var(--mf-success);border-radius:var(--mf-radius-sm);padding:11px 14px;box-shadow:var(--mf-shadow);font-size:var(--mf-fs-sm);color:var(--mf-text);max-width:360px}
    .mf-toast-err{border-left-color:var(--mf-danger)}
    .mf-toast-x{margin-left:auto;background:none;border:0;color:var(--mf-text-faint);cursor:pointer;font-size:16px;line-height:1}
  `],
})
export class ToastHost {
  readonly toasts = inject(ToastService);
}
