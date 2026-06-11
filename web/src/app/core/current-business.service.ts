import { Injectable, signal } from '@angular/core';

const KEY = 'mf-current-business';

@Injectable({ providedIn: 'root' })
export class CurrentBusinessService {
  readonly businessId = signal<string | null>(this.read());
  private read(): string | null {
    try { return localStorage.getItem(KEY); } catch { return null; }
  }
  set(id: string): void {
    this.businessId.set(id);
    try { localStorage.setItem(KEY, id); } catch { /* ignore */ }
  }
}
