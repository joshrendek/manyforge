import { Component, OnInit, inject, input, signal } from '@angular/core';
import { HttpErrorResponse } from '@angular/common/http';
import { PortalDomain, PortalDomainService } from '../../core/portal-domain.service';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';

// Settings section: the business's portal custom domain(s). List, add, verify,
// remove. Embedded in the inbox settings page; takes the businessId as an input.
// First page only (limit 50) — no paging UI.
@Component({
  selector: 'app-portal-domains',
  imports: [StatusPill, EmptyState, Spinner],
  template: `
    <section class="pd-wrap" data-testid="portal-domains">
      <h2 class="pd-head">Portal domain</h2>
      <p class="mf-hint">Serve this business's public pages on its own hostname.</p>

      <form class="pd-add" (submit)="$event.preventDefault(); add()">
        <div class="mf-field pd-field">
          <label for="portal-domain-input">Hostname</label>
          <input
            type="text"
            id="portal-domain-input"
            class="mf-input"
            data-testid="portal-domain-input"
            placeholder="support.example.com"
            autocomplete="off"
            [value]="draft()"
            (input)="draft.set($any($event.target).value)"
            name="hostname"
            [disabled]="adding()"
          />
        </div>
        <button
          type="submit"
          class="mf-btn mf-btn-primary mf-btn-sm"
          data-testid="portal-domain-add"
          [disabled]="adding() || !draft().trim()"
        >
          Add
        </button>
      </form>

      @if (error()) {
        <p class="mf-err" role="alert" data-testid="portal-domain-error">{{ error() }}</p>
      }

      @if (loading()) {
        <div class="pd-loading" data-testid="portal-domain-loading">
          <mf-spinner />
          <span>Loading portal domains…</span>
        </div>
      } @else if (loadFailed()) {
        <div>
          <button
            type="button"
            class="mf-btn mf-btn-ghost mf-btn-sm"
            data-testid="portal-domain-retry"
            (click)="load()"
          >
            Retry
          </button>
        </div>
      } @else {
        <div class="mf-table">
          @for (d of domains(); track d.id) {
            <div class="mf-tr pd-row" data-testid="portal-domain-row" [attr.data-domain-id]="d.id">
              <div class="pd-main">
                <span class="pd-name">{{ d.hostname }}</span>
                <mf-status-pill
                  [tone]="d.verified ? 'success' : 'warn'"
                  [label]="d.verified ? 'Verified' : 'Pending'"
                  data-testid="portal-domain-status"
                />
              </div>

              @if (!d.verified) {
                <div class="mf-dns-challenge" data-testid="portal-domain-dns">
                  @if (d.dns.target.value) {
                    <div class="mf-dns-rec">
                      <span class="mf-dns-kind">{{ d.dns.target.type }}</span>
                      <code>{{ d.dns.target.name }}</code>
                      <code>{{ d.dns.target.value }}</code>
                    </div>
                  }
                  <div class="mf-dns-rec">
                    <span class="mf-dns-kind">TXT</span>
                    <code>{{ d.dns.verification_txt.name }}</code>
                    <code>{{ d.dns.verification_txt.value }}</code>
                  </div>
                </div>
              }

              <div class="pd-actions">
                @if (!d.verified) {
                  <button
                    type="button"
                    class="mf-btn mf-btn-ghost mf-btn-sm"
                    data-testid="portal-domain-verify"
                    [disabled]="verifyingId() !== null"
                    (click)="verify(d)"
                  >
                    {{ verifyingId() === d.id ? 'Checking DNS…' : 'Verify' }}
                  </button>
                }
                <button
                  type="button"
                  class="mf-btn mf-btn-danger mf-btn-sm"
                  data-testid="portal-domain-remove"
                  [disabled]="removingId() !== null"
                  (click)="remove(d)"
                >
                  Remove
                </button>
                @if (!d.verified && verifyHintId() === d.id) {
                  <span class="mf-hint" data-testid="portal-domain-verify-hint">
                    Not verified yet. DNS changes can take a few minutes; try again shortly.
                  </span>
                }
              </div>
            </div>
          } @empty {
            <mf-empty-state title="No portal domain yet." data-testid="portal-domain-empty" />
          }
        </div>
      }
    </section>
  `,
  styles: [
    `
      .pd-wrap {
        display: grid;
        gap: var(--mf-space-3);
        margin-top: var(--mf-space-6);
        padding-top: var(--mf-space-4);
        border-top: 1px solid var(--mf-border);
      }
      .pd-head {
        font-size: var(--mf-fs-base);
        font-weight: 600;
        color: var(--mf-text);
        margin: 0;
      }
      .pd-wrap .mf-hint,
      .pd-wrap .mf-err {
        margin: 0;
      }
      .pd-add {
        display: flex;
        gap: var(--mf-space-3);
        flex-wrap: wrap;
        align-items: flex-end;
      }
      .pd-field {
        flex: 1 1 220px;
      }
      .pd-loading {
        display: flex;
        align-items: center;
        gap: 10px;
        color: var(--mf-text-muted);
      }
      .pd-row {
        flex-direction: column;
        align-items: flex-start;
        gap: var(--mf-space-3);
        padding: var(--mf-space-3) 0;
        border-bottom: 1px solid var(--mf-border);
      }
      .pd-main,
      .pd-actions {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: var(--mf-space-2);
      }
      .pd-name {
        font-weight: 500;
        color: var(--mf-text);
        font-size: var(--mf-fs-sm);
      }
      .mf-dns-challenge {
        display: grid;
        gap: var(--mf-space-3);
        background: var(--mf-surface-2);
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius);
        padding: var(--mf-space-4);
        width: 100%;
      }
      .mf-dns-rec {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: var(--mf-space-2);
      }
      .mf-dns-kind {
        font-size: var(--mf-fs-xs);
        font-weight: 600;
        letter-spacing: 0.03em;
        text-transform: uppercase;
        color: var(--mf-text-muted);
        min-width: 92px;
      }
      .mf-dns-rec code {
        font-size: var(--mf-fs-xs);
        word-break: break-all;
        max-width: 100%;
        background: var(--mf-surface);
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        padding: 2px var(--mf-space-2);
      }
    `,
  ],
})
export class PortalDomainsComponent implements OnInit {
  private api = inject(PortalDomainService);

  readonly businessId = input.required<string>();

  domains = signal<PortalDomain[]>([]);
  loading = signal(true);
  loadFailed = signal(false);
  adding = signal(false);
  verifyingId = signal<string | null>(null);
  verifyHintId = signal<string | null>(null);
  removingId = signal<string | null>(null);
  error = signal('');

  draft = signal('');

  ngOnInit(): void {
    this.load();
  }

  load(): void {
    this.loading.set(true);
    this.loadFailed.set(false);
    this.error.set('');
    this.api.list(this.businessId()).subscribe({
      next: (page) => {
        this.domains.set(page.items ?? []);
        this.loading.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.loadFailed.set(true);
        this.error.set(this.describeError(e));
      },
    });
  }

  add(): void {
    const hostname = this.draft().trim();
    if (!hostname || this.adding()) return;
    this.adding.set(true);
    this.error.set('');
    this.api.create(this.businessId(), hostname).subscribe({
      next: (created) => {
        this.domains.update((cur) => [created, ...cur]);
        this.draft.set('');
        this.adding.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.adding.set(false);
        this.error.set(this.describeError(e));
      },
    });
  }

  // Idempotent: a still-unverified result is a pending poll (hint), not an error.
  verify(d: PortalDomain): void {
    if (this.verifyingId() !== null) return;
    this.verifyingId.set(d.id);
    this.verifyHintId.set(null);
    this.error.set('');
    this.api.verify(this.businessId(), d.id).subscribe({
      next: (updated) => {
        this.domains.update((cur) => cur.map((x) => (x.id === updated.id ? updated : x)));
        this.verifyingId.set(null);
        if (!updated.verified) this.verifyHintId.set(updated.id);
      },
      error: (e: HttpErrorResponse) => {
        this.verifyingId.set(null);
        this.error.set(this.describeError(e));
      },
    });
  }

  remove(d: PortalDomain): void {
    if (this.removingId() !== null) return;
    if (!globalThis.confirm(`Remove ${d.hostname}? Its pages stop being served there.`)) return;
    this.removingId.set(d.id);
    this.error.set('');
    this.api.remove(this.businessId(), d.id).subscribe({
      next: () => {
        this.domains.update((cur) => cur.filter((x) => x.id !== d.id));
        this.removingId.set(null);
      },
      error: (e: HttpErrorResponse) => {
        this.removingId.set(null);
        this.error.set(this.describeError(e));
      },
    });
  }

  private describeError(e: HttpErrorResponse): string {
    if (e.status === 409) return 'That hostname is already verified by another business.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg || "That hostname isn't valid.";
    }
    return 'Something went wrong. Try again.';
  }
}
