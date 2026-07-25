import { HttpErrorResponse } from '@angular/common/http';
import { Component, ElementRef, OnInit, inject, signal, viewChild } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { AnalyticsService, TelemetryClient } from '../../core/analytics.service';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import { Business } from '../../core/tree';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';

// Analytics sites: register a site, get an embed snippet, revoke when done.
//
// The embed snippet is the whole point of this screen, so it is shown inline on every active
// site rather than hidden behind a detail page — the common task is "copy the tag onto another
// business's site", and that should be one click from the list.
@Component({
  selector: 'app-analytics-sites-list',
  imports: [FormsModule, RouterLink, PageHeader, EmptyState, Spinner, StatusPill],
  template: `
    <div class="mf-card" data-testid="analytics-sites-page">
      <mf-page-header title="Analytics sites" subtitle="Register a site, then paste its embed tag">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="sites-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      <div class="mf-filters">
        <div class="mf-field" style="flex:1 1 220px">
          <label for="an-biz-select">Business</label>
          <select
            id="an-biz-select"
            class="mf-select"
            data-testid="business-select"
            [ngModel]="businessId()"
            (ngModelChange)="selectBusiness($event)"
            name="biz"
          >
            <option value="" disabled>Choose a business…</option>
            @for (b of businesses(); track b.id) {
              <option [value]="b.id">{{ b.is_tenant_root ? b.name + ' (master)' : b.name }}</option>
            }
          </select>
        </div>
      </div>

      @if (businessId()) {
        <form class="mf-filters" data-testid="site-new" (ngSubmit)="create()">
          <div class="mf-field" style="flex:1 1 260px">
            <label for="an-new-name">New site name</label>
            <input
              id="an-new-name"
              class="mf-input"
              type="text"
              name="newName"
              placeholder="e.g. garden.gg"
              [(ngModel)]="newName"
            />
          </div>
          <div style="display:flex;align-items:flex-end">
            <button
              #createBtn
              type="submit"
              class="mf-btn mf-btn-primary mf-btn-sm"
              data-testid="site-create"
              [disabled]="!newName.trim() || creating()"
            >
              {{ creating() ? 'Creating…' : 'Add site' }}
            </button>
          </div>
        </form>
      }

      <div class="mf-table" data-testid="sites-list" role="table" aria-label="Analytics sites">
        <div class="mf-tr mf-th" role="row">
          <span style="flex:2" role="columnheader">Site</span>
          <span style="flex:3" role="columnheader">Embed tag</span>
          <span style="flex:1" role="columnheader">Status</span>
          <span style="flex:1" role="columnheader">Actions</span>
        </div>
        @for (c of sites(); track c.id) {
          <div class="mf-tr" role="row" data-testid="site-row" [attr.data-site-id]="c.id">
            <span style="flex:2" role="cell" data-testid="site-name-cell">
              @if (c.status === 'active') {
                <a [routerLink]="['/analytics', businessId(), c.id]">{{ c.name }}</a>
              } @else {
                {{ c.name }}
              }
            </span>
            <span style="flex:3" role="cell">
              @if (c.status === 'active') {
                <code class="mf-embed" data-testid="site-embed">{{ embed(c) }}</code>
                <button
                  type="button"
                  class="mf-btn mf-btn-sm"
                  data-testid="site-embed-copy"
                  [attr.aria-label]="'Copy embed tag for ' + c.name"
                  (click)="copyEmbed(c)"
                >
                  Copy
                </button>
              } @else {
                <span class="mf-muted">—</span>
              }
            </span>
            <span style="flex:1" role="cell" data-testid="site-status-cell">
              @if (c.status === 'active') {
                <mf-status-pill tone="success" label="Active" />
              } @else {
                <mf-status-pill tone="neutral" label="Revoked" />
              }
            </span>
            <span style="flex:1" role="cell">
              @if (c.status === 'active') {
                <button
                  type="button"
                  class="mf-btn mf-btn-sm mf-btn-danger"
                  data-testid="site-revoke"
                  [attr.aria-label]="'Revoke ' + c.name"
                  (click)="revoke(c)"
                >
                  Revoke
                </button>
              }
            </span>
          </div>
        }
        @if (!sites().length && businessId() && !loading()) {
          <mf-empty-state title="No sites yet" data-testid="sites-empty">
            Add one above, then paste its embed tag into your site's HTML.
          </mf-empty-state>
        }
      </div>

      @if (error()) {
        <p class="mf-err" data-testid="sites-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .mf-loading-row {
        display: flex;
        align-items: center;
        gap: 10px;
      }
      .mf-embed {
        display: inline-block;
        max-width: 100%;
        /* Wrap rather than truncate: an embed tag the user cannot fully see is one they cannot
           verify before pasting it into their own site. */
        word-break: break-all;
        overflow-wrap: anywhere;
        font-size: var(--mf-fs-xs);
        background: var(--mf-surface-2);
        padding: 2px 6px;
        border-radius: 4px;
        margin-right: 8px;
      }
      .mf-muted {
        color: var(--mf-text-muted);
      }
    `,
  ],
})
export class AnalyticsSitesListComponent implements OnInit {
  private bizApi = inject(BusinessService);
  private api = inject(AnalyticsService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  private createBtn = viewChild<ElementRef<HTMLButtonElement>>('createBtn');

  businesses = signal<Business[]>([]);
  businessId = signal<string>('');
  sites = signal<TelemetryClient[]>([]);
  loading = signal(false);
  error = signal('');
  newName = '';
  creating = signal(false);

  ngOnInit(): void {
    this.bizApi.list().subscribe({
      next: (r) => {
        const items = r.items ?? [];
        this.businesses.set(items);
        const id = this.current.businessId() ?? items[0]?.id;
        if (id) {
          this.businessId.set(id);
          this.current.set(id);
          this.reload();
        }
      },
      error: () => this.error.set('Could not load businesses'),
    });
  }

  selectBusiness(id: string): void {
    this.businessId.set(id);
    this.current.set(id);
    this.reload();
  }

  reload(): void {
    if (!this.businessId()) return;
    const biz = this.businessId();
    this.loading.set(true);
    this.api.listClients(biz).subscribe({
      next: (r) => {
        if (this.businessId() !== biz) return;
        // This screen is analytics-only; crash clients belong to a different surface.
        this.sites.set((r.clients ?? []).filter((c) => c.kind === 'analytics'));
        this.error.set('');
        this.loading.set(false);
      },
      error: () => {
        if (this.businessId() !== biz) return;
        this.sites.set([]);
        this.error.set('Could not load sites');
        this.loading.set(false);
      },
    });
  }

  embed(c: TelemetryClient): string {
    return this.api.embedSnippet(c.publishable_key);
  }

  copyEmbed(c: TelemetryClient): void {
    const tag = this.embed(c);
    navigator.clipboard?.writeText(tag).then(
      () => this.toast.success('Embed tag copied'),
      () => this.toast.error('Could not copy — select the tag and copy manually'),
    );
  }

  // A site is always created WITHOUT a signing secret. The mfs_ secret is server-to-server only
  // and would have to be embedded in a public web page to be used here, which would leak it to
  // every visitor — so the signed tier is deliberately not offered on this screen.
  create(): void {
    const name = this.newName.trim();
    if (!name || this.creating()) return;
    this.creating.set(true);
    this.api
      .createClient(this.businessId(), { kind: 'analytics', name, require_signature: false })
      .subscribe({
        next: () => {
          this.newName = '';
          this.creating.set(false);
          this.toast.success('Site added — copy its embed tag');
          this.reload();
          this.createBtn()?.nativeElement.focus();
        },
        error: (e: HttpErrorResponse) => {
          this.creating.set(false);
          this.toast.error(e.status === 400 ? 'That site name is not valid' : 'Could not add site');
        },
      });
  }

  revoke(c: TelemetryClient): void {
    this.api.revokeClient(this.businessId(), c.id).subscribe({
      next: () => {
        this.toast.success('Site revoked — its embed tag will stop collecting');
        this.reload();
      },
      error: () => this.toast.error('Could not revoke site'),
    });
  }
}
