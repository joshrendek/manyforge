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

const MODES: EmailDomainMode[] = ['forward_in', 'subdomain_mx', 'provider_route'];

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
  imports: [FormsModule, RouterLink],
  template: `
    <section class="card">
      <div class="spread">
        <div>
          <h1>Inbox settings</h1>
          <p class="sub">Custom email domains and inbound addresses for this business.</p>
        </div>
        <a class="linklike" routerLink="/support" data-testid="back-to-support">Back to tickets</a>
      </div>

      @if (loading()) {
        <p class="empty" data-testid="settings-loading">Loading inbox settings…</p>
      } @else if (loadFailed()) {
        <div class="empty">
          <p data-testid="settings-load-error">
            {{ error() || "We couldn't load inbox settings." }}
          </p>
          <button class="ghost compact" (click)="reload()">Try again</button>
        </div>
      } @else {
        <!-- ── Email domains ─────────────────────────────────────────────── -->
        <h2 class="section-head">Email domains</h2>

        <form class="add-form" data-testid="add-domain-form" (ngSubmit)="addDomain()">
          <input
            type="text"
            data-testid="domain-input"
            placeholder="support.acme.com"
            autocomplete="off"
            [(ngModel)]="domainDraft"
            name="domain"
            [disabled]="saving()"
          />
          <select
            data-testid="mode-select"
            [(ngModel)]="modeDraft"
            name="mode"
            [disabled]="saving()"
          >
            @for (m of modes; track m) {
              <option [value]="m">{{ m }}</option>
            }
          </select>
          <button
            type="submit"
            class="primary compact"
            data-testid="add-domain-submit"
            [disabled]="saving() || !domainDraft.trim()"
          >
            {{ saving() ? 'Adding…' : 'Add domain' }}
          </button>
        </form>

        <ul class="tree" data-testid="email-domain-list">
          @for (d of domains(); track d.id) {
            <li class="biz domain-row" data-testid="domain-row" [attr.data-domain-id]="d.id">
              <div class="biz-main">
                <span class="name" data-testid="domain-name">{{ d.domain }}</span>
                <span class="badge" data-testid="domain-mode">{{ d.mode }}</span>
                @if (d.verification === 'verified') {
                  <span class="pill ok" data-testid="domain-status">{{ d.verification }}</span>
                } @else {
                  <span class="badge" data-testid="domain-status">{{ d.verification }}</span>
                }
                <span class="badge" data-testid="dkim-state">DKIM: {{ d.dkim_state }}</span>
                <span class="badge" data-testid="spf-state">SPF: {{ d.spf_state }}</span>
              </div>

              @if (d.verification !== 'verified') {
                <div class="panel dns-challenge" data-testid="dns-challenge">
                  <p class="challenge-intro">
                    Publish these DNS records, then verify. Verification reads public DNS — changes
                    can take a while to propagate.
                  </p>

                  <div class="dns-rec">
                    <span class="dns-kind">Ownership (TXT)</span>
                    <code data-testid="challenge-txt-name">{{
                      d.dns_challenge.verification_txt.name
                    }}</code>
                    <code data-testid="challenge-txt-value">{{
                      d.dns_challenge.verification_txt.value
                    }}</code>
                  </div>

                  <div class="dns-rec">
                    <span class="dns-kind">DKIM (TXT)</span>
                    <code data-testid="challenge-dkim-name">{{
                      d.dns_challenge.dkim_record.name
                    }}</code>
                    <code data-testid="challenge-dkim-value">{{
                      d.dns_challenge.dkim_record.value
                    }}</code>
                  </div>

                  <div class="dns-rec">
                    <span class="dns-kind">SPF</span>
                    <code data-testid="challenge-spf-hint">{{ d.dns_challenge.spf_hint }}</code>
                  </div>

                  @if (d.dns_challenge.mx_hint) {
                    <div class="dns-rec">
                      <span class="dns-kind">MX</span>
                      <code data-testid="challenge-mx-hint">{{ d.dns_challenge.mx_hint }}</code>
                    </div>
                  }

                  <div class="challenge-actions">
                    <button
                      type="button"
                      class="ghost compact"
                      data-testid="verify-domain"
                      [disabled]="saving()"
                      (click)="verify(d)"
                    >
                      {{ verifyingId() === d.id ? 'Checking DNS…' : 'Verify' }}
                    </button>
                    @if (verifyHintId() === d.id) {
                      <span class="msg verify-hint" data-testid="verify-hint">
                        Not verified yet — re-check that the records are published and try again.
                      </span>
                    }
                  </div>
                </div>
              }
            </li>
          } @empty {
            <li class="empty" data-testid="domain-empty">No custom email domains yet.</li>
          }
        </ul>

        <!-- ── Inbound addresses ─────────────────────────────────────────── -->
        <h2 class="section-head">Inbound addresses</h2>

        <form class="add-form" data-testid="add-address-form" (ngSubmit)="addAddress()">
          <input
            type="text"
            data-testid="address-input"
            placeholder="hello@support.acme.com"
            autocomplete="off"
            [(ngModel)]="addressDraft"
            name="address"
            [disabled]="saving()"
          />
          <select
            data-testid="address-domain-select"
            [(ngModel)]="selectedDomainId"
            name="email_domain_id"
            [disabled]="saving() || !verifiedDomains().length"
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
          <button
            type="submit"
            class="primary compact"
            data-testid="add-address-submit"
            [disabled]="saving() || !addressDraft.trim() || !selectedDomainId"
          >
            {{ saving() ? 'Adding…' : 'Add address' }}
          </button>
        </form>

        <ul class="tree" data-testid="inbound-address-list">
          @for (a of addresses(); track a.id) {
            <li class="biz address-row" data-testid="address-row" [attr.data-address-id]="a.id">
              <div class="biz-main">
                <span class="name" data-testid="address-value">{{ a.address }}</span>
                <span class="badge" data-testid="address-kind">{{ a.kind }}</span>
                @if (!a.active) {
                  <span class="badge" data-testid="address-inactive">inactive</span>
                }
              </div>
            </li>
          } @empty {
            <li class="empty" data-testid="address-empty">No inbound addresses yet.</li>
          }
        </ul>
      }

      @if (error() && !loadFailed()) {
        <p class="msg error" data-testid="settings-error">{{ error() }}</p>
      }
    </section>
  `,
  styles: [
    `
      .section-head {
        margin-top: 22px;
        padding-top: 18px;
        border-top: 1px solid var(--border);
      }
      .section-head:first-of-type {
        margin-top: 8px;
        padding-top: 0;
        border-top: 0;
      }
      .add-form {
        display: flex;
        gap: 10px;
        flex-wrap: wrap;
        align-items: center;
        margin-top: 12px;
      }
      .add-form input {
        flex: 1 1 220px;
        margin: 0;
      }
      .add-form select {
        flex: 0 1 200px;
        margin: 0;
      }
      .domain-row,
      .address-row {
        align-items: flex-start;
      }
      .biz-main {
        flex-wrap: wrap;
      }
      .dns-challenge {
        display: grid;
        gap: 10px;
      }
      .challenge-intro {
        margin: 0;
        color: var(--muted);
        font-size: 12.5px;
        line-height: 1.5;
      }
      .dns-rec {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: 8px;
      }
      .dns-rec .dns-kind {
        font-size: 10.5px;
        font-weight: 600;
        letter-spacing: 0.03em;
        text-transform: uppercase;
        color: var(--muted);
        min-width: 92px;
      }
      .dns-rec code {
        font-size: 11.5px;
        word-break: break-all;
        max-width: 100%;
      }
      .challenge-actions {
        display: flex;
        align-items: center;
        gap: 12px;
        flex-wrap: wrap;
      }
      .verify-hint {
        margin: 0;
        color: var(--muted);
      }
      .pill.ok {
        color: var(--ok);
        background: transparent;
        border-color: var(--ok);
      }
    `,
  ],
})
export class InboxSettingsComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private api = inject(TicketService);

  readonly modes = MODES;

  private businessId = '';

  domains = signal<EmailDomain[]>([]);
  addresses = signal<InboundAddress[]>([]);
  loading = signal(true);
  loadFailed = signal(false);
  saving = signal(false);
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
    if (!domain || this.saving()) return;
    this.saving.set(true);
    this.error.set('');
    this.api.createEmailDomain(this.businessId, { domain, mode: this.modeDraft }).subscribe({
      next: (created) => {
        this.domains.update((cur) => [...cur, created]);
        this.domainDraft = '';
        this.modeDraft = 'forward_in';
        this.saving.set(false);
      },
      error: (e: HttpErrorResponse) => {
        this.saving.set(false);
        this.error.set(this.describeDomainError(e));
      },
    });
  }

  // Trigger verification. The endpoint is idempotent and returns the current state:
  // a domain still unverified is a pending poll (NO error) — we surface a "re-check
  // DNS" hint rather than an error banner. Reflect the returned domain in place.
  verify(d: EmailDomain): void {
    if (this.saving()) return;
    this.saving.set(true);
    this.verifyingId.set(d.id);
    this.verifyHintId.set(null);
    this.error.set('');
    this.api.verifyEmailDomain(this.businessId, d.id).subscribe({
      next: (updated) => {
        this.domains.update((cur) => cur.map((x) => (x.id === updated.id ? updated : x)));
        this.saving.set(false);
        this.verifyingId.set(null);
        if (updated.verification !== 'verified') {
          this.verifyHintId.set(updated.id);
        }
      },
      error: (e: HttpErrorResponse) => {
        this.saving.set(false);
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
    if (!address || !domainId || this.saving()) return;
    this.saving.set(true);
    this.error.set('');
    this.api
      .createInboundAddress(this.businessId, { address, email_domain_id: domainId })
      .subscribe({
        next: (created) => {
          this.addresses.update((cur) => [...cur, created]);
          this.addressDraft = '';
          this.selectedDomainId = '';
          this.saving.set(false);
        },
        error: (e: HttpErrorResponse) => {
          this.saving.set(false);
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
