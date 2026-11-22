import { DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, DestroyRef, inject, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';
import { AuthService } from '../core/auth.service';
import { Passkey, PasskeyService, supportsPasskeys } from '../core/passkey.service';

@Component({
  selector: 'app-account-security',
  imports: [DatePipe, FormsModule],
  template: `
    <header class="mf-pageheader">
      <h1>Account security</h1>
    </header>
    <section class="mf-card" aria-labelledby="passkeys-heading">
      <h2 id="passkeys-heading">Passkeys</h2>
      <p class="mf-pageheader-sub">Sign in with your device, password manager, or security key.</p>

      @if (loading()) {
        <p role="status">Loading passkeys…</p>
      } @else if (unavailable()) {
        <p>Passkeys are unavailable for this account.</p>
      } @else if (loaded()) {
        @if (!canManage()) {
          <div class="passkey-actions">
            <p>Sign in again to add or remove passkeys.</p>
            <button class="mf-btn mf-btn-ghost" type="button" [disabled]="busy()" (click)="signInAgain()">Sign in again</button>
          </div>
        }
        @if (items().length) {
          <ul class="passkey-list">
            @for (item of items(); track item.id) {
              <li>
                <div>
                  <strong>{{ item.name }}</strong>
                  <div class="passkey-dates">
                    Added {{ item.created_at | date: 'mediumDate' }}
                    @if (item.last_used_at) { · Last used {{ item.last_used_at | date: 'mediumDate' }} }
                  </div>
                </div>
                @if (removing() === item.id) {
                  <div class="passkey-actions">
                    <button class="mf-btn mf-btn-ghost" type="button" [disabled]="busy()" (click)="removing.set(null)">Cancel</button>
                    <button class="mf-btn mf-btn-danger" type="button" [disabled]="busy()" (click)="remove(item)">Confirm removal</button>
                  </div>
                } @else {
                  <button class="mf-btn mf-btn-ghost" type="button" [disabled]="busy() || !canManage()"
                    [attr.aria-label]="'Remove ' + item.name" (click)="removing.set(item.id)">Remove</button>
                }
              </li>
            }
          </ul>
        } @else {
          <p>No passkeys added.</p>
        }
        @if (!supported) {
          <p>Use an up-to-date browser over a secure connection to add a passkey.</p>
        } @else {
          <form class="passkey-actions" (ngSubmit)="add()">
            <div class="mf-field">
              <label for="passkey-name">Passkey name</label>
              <input class="mf-input" id="passkey-name" name="passkeyName" [(ngModel)]="name"
                [disabled]="busy() || !canManage()" placeholder="e.g. Work laptop" maxlength="80" required />
            </div>
            <button class="mf-btn mf-btn-primary" type="submit" [disabled]="busy() || !canManage() || !name.trim()">Add passkey</button>
          </form>
        }
      }

      @if (error()) { <p class="mf-err" role="alert">{{ error() }}</p> }
      @if (status()) { <p role="status">{{ status() }}</p> }
      @if (!loaded() && !loading() && !unavailable()) {
        <button class="mf-btn mf-btn-ghost" type="button" (click)="load()">Try again</button>
      }
    </section>
  `,
  styles: [`
    .mf-card { max-width: 720px; }
    h2 { margin-top: 0; }
    .passkey-actions { display: flex; align-items: end; flex-wrap: wrap; gap: 10px; }
    .passkey-actions p { margin: 0; align-self: center; }
    .passkey-list { list-style: none; padding: 0; }
    .passkey-list li { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 12px; padding: 12px 0; }
    .passkey-list strong { overflow-wrap: anywhere; }
    .passkey-dates { color: var(--mf-text-muted); }
  `],
})
export class AccountSecurityComponent {
  private readonly passkeys = inject(PasskeyService);
  private readonly auth = inject(AuthService);
  private readonly router = inject(Router);
  private readonly destroyRef = inject(DestroyRef);
  private readonly passkeyPrompt = new AbortController();

  readonly supported = supportsPasskeys();
  readonly items = signal<Passkey[]>([]);
  readonly loading = signal(false);
  readonly loaded = signal(false);
  readonly unavailable = signal(false);
  readonly canManage = signal(false);
  readonly busy = signal(false);
  readonly removing = signal<string | null>(null);
  readonly error = signal('');
  readonly status = signal('');
  name = '';

  constructor() {
    this.destroyRef.onDestroy(() => this.passkeyPrompt.abort());
    this.load();
  }

  load(): void {
    this.loading.set(true);
    this.error.set('');
    this.passkeys.list().pipe(takeUntilDestroyed(this.destroyRef)).subscribe({
      next: (result) => {
        this.items.set(result.items);
        this.canManage.set(result.can_manage);
        this.loaded.set(true);
        this.loading.set(false);
      },
      error: (err: HttpErrorResponse) => {
        this.loading.set(false);
        if (err.status === 404) {
          this.unavailable.set(true);
        } else {
          this.error.set('Could not load passkeys.');
        }
      },
    });
  }

  async signInAgain(): Promise<void> {
    this.busy.set(true);
    try {
      if (await this.router.navigate(['/login'], { queryParams: { returnTo: 'security' } })) {
        this.auth.logout().subscribe({ error: () => {} });
      }
    } finally {
      this.busy.set(false);
    }
  }

  add(): void {
    const name = this.name.trim();
    if (this.busy() || !this.canManage() || !name) return;
    this.startChange();
    this.passkeys.register(name, this.passkeyPrompt.signal).pipe(takeUntilDestroyed(this.destroyRef)).subscribe({
      next: (key) => {
        this.items.update((items) => [...items, key]);
        this.name = '';
        this.busy.set(false);
        this.status.set('Passkey added.');
      },
      error: (err: unknown) => {
        const canceled = err instanceof DOMException && err.name === 'NotAllowedError';
        const conflict = err instanceof HttpErrorResponse && err.status === 409;
        this.changeFailed(err, canceled ? 'Passkey setup was canceled or timed out.'
          : conflict ? 'This passkey is already registered or the account has 20 passkeys.'
          : 'Could not add the passkey. Try again.');
      },
    });
  }

  remove(item: Passkey): void {
    if (this.busy() || !this.canManage()) return;
    this.startChange();
    this.passkeys.remove(item.id).pipe(takeUntilDestroyed(this.destroyRef)).subscribe({
      next: () => {
        this.items.update((items) => items.filter((key) => key.id !== item.id));
        this.busy.set(false);
        this.status.set('Passkey removed.');
      },
      error: (err: unknown) => this.changeFailed(err, 'Could not remove the passkey. Try again.'),
    });
  }

  private startChange(): void {
    this.busy.set(true);
    this.error.set('');
    this.status.set('');
    this.removing.set(null);
  }

  private changeFailed(err: unknown, message: string): void {
    this.busy.set(false);
    if (err instanceof HttpErrorResponse && err.error?.code === 'REAUTHENTICATION_REQUIRED') {
      this.canManage.set(false);
    } else {
      this.error.set(message);
    }
  }
}
