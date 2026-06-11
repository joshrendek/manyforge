import { Component, OnInit, computed, inject, signal } from '@angular/core';
import { HttpErrorResponse } from '@angular/common/http';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute, RouterLink } from '@angular/router';
import {
  EmailDomain,
  EmailDomainMode,
  InboundAddress,
  TicketService,
} from '../../core/ticket.service';
import { PageHeader } from '../../ui/page-header/page-header';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { EmptyState } from '../../ui/empty-state/empty-state';
import { Spinner } from '../../ui/spinner/spinner';
import { Tone } from '../../ui/status';

const MODES: EmailDomainMode[] = ['forward_in', 'subdomain_mx', 'provider_route'];

function verificationTone(v: string): Tone {
  switch (v) {
    case 'verified': return 'success';
    case 'pending': return 'warn';
    case 'failed': return 'danger';
    default: return 'neutral';
  }
}

function dkimSpfTone(s: string): Tone {
  switch (s) {
    case 'pass': return 'success';
    case 'fail': return 'danger';
    case 'pending': return 'warn';
    default: return 'neutral';
  }
}

// US4 inbox-settings page. Manages a business's custom email domains and inbound
// addresses (FR-012/FR-013). Mirrors thread-view.ts / ticket-list.ts: signal-driven
// state, a centralised reload(), generic no-oracle error copy, [disabled] gating on
// every input during a mutation. The businessId is taken from the route
// (/support/:businessId/settings/inbox) — the caller arrives here already scoped.
//
// Three sections: (1) add-domain form (domain + mode); (2) the domain list, each
// row carrying verification/DKIM/SPF state plus, for UNVERIFIED domains, the DNS
// challenge records to publish and a Verify button; (3) the inbound-address list
// (system + custom) with an add-address form whose domain <select> is the business's
// VERIFIED domains only (the backend 409s on an unverified domain regardless).
@Component({
  selector: 'app-inbox-settings',
  imports: [FormsModule, RouterLink, PageHeader, StatusPill, EmptyState, Spinner],
  template: `
    <div class="mf-card">
      <mf-page-header
        title="Inbox settings"
        subtitle="Custom email domains and inbound addresses for this business."
      >
        <ng-container actions>
          <a
            class="mf-btn mf-btn-ghost mf-btn-sm"
            routerLink="/support"
            data-testid="back-to-support"
            >Back to tickets</a
          >
        </ng-container>
      </mf-page-header>

      @if (loading()) {
        <div data-testid="settings-loading" style="display:flex;align-items:center;gap:10px;color:var(--mf-text-muted)">
          <mf-spinner />
          <span>Loading inbox settings…</span>
        </div>
      } @else if (loadFailed()) {
        <div style="padding:var(--mf-space-4);text-align:center">
          <p class="mf-err" data-testid="settings-load-error">
            {{ error() || "We couldn't load inbox settings." }}
          </p>
          <button class="mf-btn mf-btn-ghost mf-btn-sm" (click)="reload()">Try again</button>
        </div>
      } @else {
        <!-- ── Email domains ─────────────────────────────────────────────── -->
        <h2 class="mf-section-head">Email domains</h2>

        <form class="mf-add-form" data-testid="add-domain-form" (ngSubmit)="addDomain()">
          <div class="mf-field" style="flex:1 1 220px">
            <label for="domain-input">Domain</label>
            <input
              type="text"
              id="domain-input"
              class="mf-input"
              data-testid="domain-input"
              placeholder="support.acme.com"
              autocomplete="off"
              [(ngModel)]="domainDraft"
              name="domain"
              [disabled]="addingDomain()"
            />
          </div>
          <div class="mf-field" style="flex:0 1 200px">
            <label for="mode-select">Mode</label>
            <select
              id="mode-select"
              class="mf-select"
              data-testid="mode-select"
              [(ngModel)]="modeDraft"
              name="mode"
              [disabled]="addingDomain()"
            >
              @for (m of modes; track m) {
                <option [value]="m">{{ m }}</option>
              }
            </select>
          </div>
          <button
            type="submit"
            class="mf-btn mf-btn-primary mf-btn-sm"
            data-testid="add-domain-submit"
            [disabled]="addingDomain() || !domainDraft.trim()"
          >
            {{ addingDomain() ? 'Adding…' : 'Add domain' }}
          </button>
        </form>

        <div class="mf-table" data-testid="email-domain-list">
          @for (d of domains(); track d.id) {
            <div class="mf-tr mf-domain-row" data-testid="domain-row" [attr.data-domain-id]="d.id">
              <div class="mf-row-main">
                <span class="mf-row-name" data-testid="domain-name">{{ d.domain }}</span>
                <mf-status-pill
                  [tone]="'neutral'"
                  [label]="d.mode"
                  data-testid="domain-mode"
                />
                <mf-status-pill
                  [tone]="verificationTone(d.verification)"
                  [label]="d.verification"
                  data-testid="domain-status"
                />
                <mf-status-pill
                  [tone]="dkimSpfTone(d.dkim_state)"
                  [label]="'DKIM: ' + d.dkim_state"
                  data-testid="dkim-state"
                />
                <mf-status-pill
                  [tone]="dkimSpfTone(d.spf_state)"
                  [label]="'SPF: ' + d.spf_state"
                  data-testid="spf-state"
                />
              </div>

              @if (d.verification !== 'verified') {
                <div class="mf-dns-challenge" data-testid="dns-challenge">
                  <p class="mf-dns-intro">
                    Publish these DNS records, then verify. Verification reads public DNS — changes
                    can take a while to propagate.
                  </p>

                  <div class="mf-dns-rec">
                    <span class="mf-dns-kind">Ownership (TXT)</span>
                    <code data-testid="challenge-txt-name">{{
                      d.dns_challenge.verification_txt.name
                    }}</code>
                    <code data-testid="challenge-txt-value">{{
                      d.dns_challenge.verification_txt.value
                    }}</code>
                  </div>

                  <div class="mf-dns-rec">
                    <span class="mf-dns-kind">DKIM (TXT)</span>
                    <code data-testid="challenge-dkim-name">{{
                      d.dns_challenge.dkim_record.name
                    }}</code>
                    <code data-testid="challenge-dkim-value">{{
                      d.dns_challenge.dkim_record.value
                    }}</code>
                  </div>

                  <div class="mf-dns-rec">
                    <span class="mf-dns-kind">SPF</span>
                    <code data-testid="challenge-spf-hint">{{ d.dns_challenge.spf_hint }}</code>
                  </div>

                  @if (d.dns_challenge.mx_hint) {
                    <div class="mf-dns-rec">
                      <span class="mf-dns-kind">MX</span>
                      <code data-testid="challenge-mx-hint">{{ d.dns_challenge.mx_hint }}</code>
                    </div>
                  }

                  <div class="mf-challenge-actions">
                    <button
                      type="button"
                      class="mf-btn mf-btn-ghost mf-btn-sm"
                      data-testid="verify-domain"
                      [disabled]="verifyingId() !== null"
                      (click)="verify(d)"
                    >
                      {{ verifyingId() === d.id ? 'Checking DNS…' : 'Verify' }}
                    </button>
                    @if (verifyHintId() === d.id) {
                      <span class="mf-hint" data-testid="verify-hint">
                        Not verified yet — re-check that the records are published and try again.
                      </span>
                    }
                  </div>
                </div>
              }
            </div>
          } @empty {
            <mf-empty-state title="No custom email domains yet." data-testid="domain-empty" />
          }
        </div>

        <!-- ── Inbound addresses ─────────────────────────────────────────── -->
        <h2 class="mf-section-head">Inbound addresses</h2>

        <form class="mf-add-form" data-testid="add-address-form" (ngSubmit)="addAddress()">
          <div class="mf-field" style="flex:1 1 220px">
            <label for="address-input">Address</label>
            <input
              type="text"
              id="address-input"
              class="mf-input"
              data-testid="address-input"
              placeholder="hello@support.acme.com"
              autocomplete="off"
              [(ngModel)]="addressDraft"
              name="address"
              [disabled]="addingAddress()"
            />
          </div>
          <div class="mf-field" style="flex:0 1 200px">
            <label for="address-domain-select">Domain</label>
            <select
              id="address-domain-select"
              class="mf-select"
              data-testid="address-domain-select"
              [(ngModel)]="selectedDomainId"
              name="email_domain_id"
              [disabled]="addingAddress() || !verifiedDomains().length"
            >
              <option value="" disabled>
                {{
                  verifiedDomains().length ? 'Choose a verified domain…' : 'No verified domains yet'
                }}
              </option>
              @for (d of verifiedDomains(); track d.id) {
                <option [value]="d.id">{{ d.domain }}</option>
              }
            </select>
          </div>
          <button
            type="submit"
            class="mf-btn mf-btn-primary mf-btn-sm"
            data-testid="add-address-submit"
            [disabled]="addingAddress() || !addressDraft.trim() || !selectedDomainId"
          >
            {{ addingAddress() ? 'Adding…' : 'Add address' }}
          </button>
        </form>

        <div class="mf-table" data-testid="inbound-address-list">
          @for (a of addresses(); track a.id) {
            <div class="mf-tr mf-address-row" data-testid="address-row" [attr.data-address-id]="a.id">
              <div class="mf-row-main">
                <span class="mf-row-name" data-testid="address-value">{{ a.address }}</span>
                <mf-status-pill [tone]="'neutral'" [label]="a.kind" data-testid="address-kind" />
                @if (!a.active) {
                  <mf-status-pill [tone]="'danger'" label="inactive" data-testid="address-inactive" />
                }
              </div>
            </div>
          } @empty {
            <mf-empty-state title="No inbound addresses yet." data-testid="address-empty" />
          }
        </div>
      }

      @if (error() && !loadFailed()) {
        <p class="mf-err" data-testid="settings-error">{{ error() }}</p>
      }
    </div>
  `,
  styles: [
    `
      .mf-section-head {
        font-size: var(--mf-fs-base);
        font-weight: 600;
        color: var(--mf-text);
        margin-top: var(--mf-space-6);
        padding-top: var(--mf-space-4);
        border-top: 1px solid var(--mf-border);
      }
      .mf-section-head:first-of-type {
        margin-top: var(--mf-space-2);
        padding-top: 0;
        border-top: 0;
      }
      .mf-add-form {
        display: flex;
        gap: var(--mf-space-3);
        flex-wrap: wrap;
        align-items: flex-end;
        margin-top: var(--mf-space-3);
      }
      .mf-domain-row,
      .mf-address-row {
        flex-direction: column;
        align-items: flex-start;
        gap: var(--mf-space-3);
        padding: var(--mf-space-3) 0;
        border-bottom: 1px solid var(--mf-border);
      }
      .mf-row-main {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: var(--mf-space-2);
      }
      .mf-row-name {
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
      .mf-dns-intro {
        margin: 0;
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
        line-height: 1.5;
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
      .mf-challenge-actions {
        display: flex;
        align-items: center;
        gap: var(--mf-space-3);
        flex-wrap: wrap;
      }
    `,
  ],
})
export class InboxSettingsComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(TicketService);

  // Template tone helpers
  readonly verificationTone = verificationTone;
  readonly dkimSpfTone = dkimSpfTone;

  readonly modes = MODES;

  private businessId = '';

  domains = signal<EmailDomain[]>([]);
  addresses = signal<InboundAddress[]>([]);
  loading = signal(true);
  loadFailed = signal(false);
  // Per-action in-flight signals so independent mutations don't block one another
  // (manyforge-mu7): a slow per-domain Verify must NOT disable the add-domain or
  // add-address forms. addingDomain/addingAddress each gate only their own form;
  // the per-row Verify buttons gate on verifyingId (below).
  addingDomain = signal(false);
  addingAddress = signal(false);
  error = signal('');

  // Tracks which domain a verify call is in-flight / surfaced a pending hint for,
  // so the spinner + "re-check DNS" hint scope to a single row.
  verifyingId = signal<string | null>(null);
  verifyHintId = signal<string | null>(null);

  // Form drafts.
  domainDraft = '';
  modeDraft: EmailDomainMode = 'forward_in';
  addressDraft = '';
  selectedDomainId = '';

  // The add-address domain <select> only offers VERIFIED domains: the backend
  // 409s on an unverified domain, so steering the operator avoids a guaranteed error.
  readonly verifiedDomains = computed(() =>
    this.domains().filter((d) => d.verification === 'verified'),
  );

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.reload();
  }

  reload(): void {
    if (!this.businessId) {
      this.loading.set(false);
      this.loadFailed.set(true);
      return;
    }
    this.loading.set(true);
    this.loadFailed.set(false);
    this.error.set('');
    this.api.listEmailDomains(this.businessId).subscribe({
      next: (page) => {
        this.domains.set(page.items ?? []);
        this.loadAddresses();
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.loadFailed.set(true);
        this.error.set(this.describeLoadError(e));
      },
    });
  }

  private loadAddresses(): void {
    this.api.listInboundAddresses(this.businessId).subscribe({
      next: (page) => {
        this.addresses.set(page.items ?? []);
        this.loading.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.loadFailed.set(true);
        this.error.set(this.describeLoadError(e));
      },
    });
  }

  // Add a custom domain. On success prepend the returned domain (it carries its
  // own DNS challenge) so the operator sees the records to publish immediately;
  // a full reload would also work but loses scroll/ordering nicety.
  addDomain(): void {
    const domain = this.domainDraft.trim();
    if (!domain || this.addingDomain()) return;
    this.addingDomain.set(true);
    this.error.set('');
    this.api.createEmailDomain(this.businessId, { domain, mode: this.modeDraft }).subscribe({
      next: (created) => {
        this.domains.update((cur) => [...cur, created]);
        this.domainDraft = '';
        this.modeDraft = 'forward_in';
        this.addingDomain.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.addingDomain.set(false);
        this.error.set(this.describeDomainError(e));
      },
    });
  }

  // Trigger verification. The endpoint is idempotent and returns the current state:
  // a domain still unverified is a pending poll (NO error) — we surface a "re-check
  // DNS" hint rather than an error banner. Reflect the returned domain in place.
  verify(d: EmailDomain): void {
    // Gate only on an in-flight verify (its own signal), so the add-domain and
    // add-address forms stay usable while DNS is being checked (manyforge-mu7).
    if (this.verifyingId() !== null) return;
    this.verifyingId.set(d.id);
    this.verifyHintId.set(null);
    this.error.set('');
    this.api.verifyEmailDomain(this.businessId, d.id).subscribe({
      next: (updated) => {
        this.domains.update((cur) => cur.map((x) => (x.id === updated.id ? updated : x)));
        this.verifyingId.set(null);
        if (updated.verification !== 'verified') {
          this.verifyHintId.set(updated.id);
        }
      },
      error: (e: HttpErrorResponse) => {
        this.verifyingId.set(null);
        this.error.set(this.describeVerifyError(e));
      },
    });
  }

  // Add a custom address on the chosen verified domain. The backend re-checks the
  // domain is owned + verified; a 409 (unverified/duplicate) is surfaced friendly.
  addAddress(): void {
    const address = this.addressDraft.trim();
    const domainId = this.selectedDomainId;
    if (!address || !domainId || this.addingAddress()) return;
    this.addingAddress.set(true);
    this.error.set('');
    this.api
      .createInboundAddress(this.businessId, { address, email_domain_id: domainId })
      .subscribe({
        next: (created) => {
          this.addresses.update((cur) => [...cur, created]);
          this.addressDraft = '';
          this.selectedDomainId = '';
          this.addingAddress.set(false);
        },
        error: (e: HttpErrorResponse) => {
          this.addingAddress.set(false);
          this.error.set(this.describeAddressError(e));
        },
      });
  }

  // ── error copy ──────────────────────────────────────────────────────────
  // No-oracle: 403/404 both map to the same generic message (mirrors the other
  // support pages). 400 carries safe validation detail; 409 is a stateful conflict.

  private describeLoadError(e: HttpErrorResponse): string {
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return "We couldn't load inbox settings.";
  }

  private describeDomainError(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg
        ? `That domain was rejected: ${msg}`
        : 'That domain was rejected. Check the value.';
    }
    if (e.status === 409) return 'That domain is already added.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not add the domain. Please try again.';
  }

  private describeVerifyError(e: HttpErrorResponse): string {
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not verify the domain. Please try again.';
  }

  private describeAddressError(e: HttpErrorResponse): string {
    if (e.status === 400) {
      const msg = (e.error as { message?: string } | null)?.message;
      return msg
        ? `That address was rejected: ${msg}`
        : 'That address was rejected. Check that it is on the selected domain.';
    }
    if (e.status === 409) return 'Verify the domain first, or that address already exists.';
    if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
    return 'Could not add the address. Please try again.';
  }
}
