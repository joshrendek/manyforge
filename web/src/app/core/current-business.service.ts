import { Injectable, computed, signal } from '@angular/core';
import { Business } from './tree';

const KEY = 'mf-current-business';

@Injectable({ providedIn: 'root' })
export class CurrentBusinessService {
  readonly businessId = signal<string | null>(this.read());
  readonly businesses = signal<Business[]>([]);
  readonly business = computed(() => this.businesses().find((b) => b.id === this.businessId()) ?? null);
  readonly businessName = computed(() => this.business()?.name ?? '');

  private read(): string | null {
    try { return localStorage.getItem(KEY); } catch { return null; }
  }

  set(id: string): void {
    this.businessId.set(id);
    try { localStorage.setItem(KEY, id); } catch { /* Storage may be unavailable. */ }
  }

  replaceBusinesses(items: Business[]): void {
    this.businesses.set(items);
    if (!items.some((b) => b.id === this.businessId())) {
      const first = items.find((b) => b.status !== 'archived');
      if (first) this.set(first.id);
      else this.clearSelection();
    }
  }

  clear(): void {
    this.businesses.set([]);
    this.clearSelection();
  }

  private clearSelection(): void {
    this.businessId.set(null);
    try { localStorage.removeItem(KEY); } catch { /* Storage may be unavailable. */ }
  }
}
