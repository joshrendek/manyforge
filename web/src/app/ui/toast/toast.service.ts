import { Injectable, signal } from '@angular/core';

export interface Toast { id: number; kind: 'success' | 'error'; message: string; }

@Injectable({ providedIn: 'root' })
export class ToastService {
  readonly toasts = signal<Toast[]>([]);
  private seq = 0;

  success(message: string) { this.push('success', message); }
  error(message: string) { this.push('error', message); }

  dismiss(id: number) { this.toasts.update((t) => t.filter((x) => x.id !== id)); }

  private push(kind: Toast['kind'], message: string) {
    const id = ++this.seq;
    this.toasts.update((t) => [...t, { id, kind, message }]);
    setTimeout(() => this.dismiss(id), 5000);
  }
}
