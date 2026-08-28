import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import {
  MailingSendingMode,
  MailingSendingProfile,
  MailingSendingProfileInput,
  MailingService,
} from '../../core/mailing.service';
import { EmailDomain, TicketService } from '../../core/ticket.service';
import { Business } from '../../core/tree';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { mailingProfileStatusTone } from '../../ui/status';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { ToastService } from '../../ui/toast/toast.service';

@Component({
  selector: 'app-mailing-sending-profile',
  standalone: true,
  imports: [FormsModule, PageHeader, Spinner, StatusPill],
  template: `
    <div class="mf-card page" data-testid="mailing-sending-profile-page">
      <mf-page-header
        title="Sending profile"
        subtitle="Choose how campaigns are sent and which identity recipients see"
      >
        @if (profile(); as currentProfile) {
          <mf-status-pill
            actions
            [tone]="profileTone(currentProfile.status)"
            [label]="statusLabel(currentProfile.status)"
            data-testid="sending-profile-status"
          />
        }
      </mf-page-header>

      <div class="mf-field business-field">
        <label for="sending-business">Business</label>
        <select
          id="sending-business"
          class="mf-select"
          data-testid="business-select"
          [ngModel]="businessId()"
          (ngModelChange)="selectBusiness($event)"
        >
          <option value="" disabled>Choose a business…</option>
          @for (business of businesses(); track business.id) {
            <option [value]="business.id">
              {{ business.is_tenant_root ? business.name + ' (master)' : business.name }}
            </option>
          }
        </select>
      </div>

      @if (loading()) {
        <p class="loading" data-testid="sending-profile-loading"><mf-spinner /> Loading…</p>
      } @else if (businessId()) {
        <form class="profile-form" data-testid="sending-profile-form" (ngSubmit)="save()">
          <fieldset class="mode-fieldset">
            <legend>Delivery provider</legend>
            <label class="mode-option">
              <input
                type="radio"
                name="mode"
                value="relay"
                data-testid="sending-mode-relay"
                [(ngModel)]="mode"
                (ngModelChange)="modeChanged()"
              />
              <span
                ><b>ManyForge relay</b
                ><small>Use a verified domain already connected here.</small></span
              >
            </label>
            <label class="mode-option">
              <input
                type="radio"
                name="mode"
                value="resend"
                data-testid="sending-mode-resend"
                [(ngModel)]="mode"
                (ngModelChange)="modeChanged()"
              />
              <span><b>Resend</b><small>Bring your own Resend API credentials.</small></span>
            </label>
            <label class="mode-option">
              <input
                type="radio"
                name="mode"
                value="ses"
                data-testid="sending-mode-ses"
                [(ngModel)]="mode"
                (ngModelChange)="modeChanged()"
              />
              <span><b>Amazon SES</b><small>Send through your AWS SES account.</small></span>
            </label>
          </fieldset>

          <div class="field-grid">
            <div class="mf-field">
              <label for="sending-from-name">From name</label>
              <input
                id="sending-from-name"
                class="mf-input"
                name="fromName"
                data-testid="sending-from-name"
                [(ngModel)]="fromName"
                maxlength="200"
                required
              />
            </div>
            <div class="mf-field">
              <label for="sending-from-email">From email</label>
              <input
                id="sending-from-email"
                class="mf-input"
                type="email"
                name="fromEmail"
                data-testid="sending-from-email"
                [(ngModel)]="fromEmail"
                required
              />
            </div>
            <div class="mf-field">
              <label for="sending-reply-to">Reply-to <span>(optional)</span></label>
              <input
                id="sending-reply-to"
                class="mf-input"
                type="email"
                name="replyTo"
                data-testid="sending-reply-to"
                [(ngModel)]="replyTo"
              />
            </div>
            @if (mode === 'relay') {
              <div class="mf-field">
                <label for="sending-domain">Verified domain</label>
                <select
                  id="sending-domain"
                  class="mf-select"
                  name="domain"
                  data-testid="sending-domain"
                  [(ngModel)]="emailDomainId"
                  required
                >
                  <option value="" disabled>Choose a verified domain…</option>
                  @for (domain of verifiedDomains(); track domain.id) {
                    <option [value]="domain.id">{{ domain.domain }}</option>
                  }
                </select>
                @if (!verifiedDomains().length) {
                  <span class="mf-hint" data-testid="sending-domain-empty">
                    Verify an email domain in Support settings before using the relay.
                  </span>
                }
              </div>
            }
          </div>

          @if (mode === 'resend') {
            <section class="provider-card" data-testid="sending-resend-fields">
              <h2>Resend credentials</h2>
              @if (credentialsStoredForMode()) {
                <div class="stored-row" data-testid="sending-credentials-stored">
                  <mf-status-pill tone="success" label="Credentials saved" />
                  <span class="mf-hint">The saved secret cannot be viewed again.</span>
                  <button
                    type="button"
                    class="mf-btn mf-btn-ghost mf-btn-sm"
                    data-testid="sending-credentials-replace"
                    (click)="replaceCredentials.set(true)"
                  >
                    Replace credentials
                  </button>
                </div>
              } @else {
                <div class="field-grid">
                  <div class="mf-field">
                    <label for="sending-resend-key">API key</label>
                    <input
                      id="sending-resend-key"
                      class="mf-input"
                      type="password"
                      name="resendKey"
                      autocomplete="new-password"
                      data-testid="sending-resend-key"
                      [(ngModel)]="resendApiKey"
                      required
                    />
                  </div>
                  <div class="mf-field">
                    <label for="sending-resend-webhook"
                      >Webhook secret <span>(optional)</span></label
                    >
                    <input
                      id="sending-resend-webhook"
                      class="mf-input"
                      type="password"
                      name="resendWebhook"
                      autocomplete="new-password"
                      data-testid="sending-resend-webhook"
                      [(ngModel)]="resendWebhookSecret"
                    />
                  </div>
                </div>
              }
            </section>
          }

          @if (mode === 'ses') {
            <section class="provider-card" data-testid="sending-ses-fields">
              <h2>Amazon SES</h2>
              @if (credentialsStoredForMode()) {
                <div class="stored-row" data-testid="sending-credentials-stored">
                  <mf-status-pill tone="success" label="Credentials saved" />
                  <span class="mf-hint">The saved secret cannot be viewed again.</span>
                  <button
                    type="button"
                    class="mf-btn mf-btn-ghost mf-btn-sm"
                    data-testid="sending-credentials-replace"
                    (click)="replaceCredentials.set(true)"
                  >
                    Replace credentials
                  </button>
                </div>
              } @else {
                <div class="field-grid">
                  <div class="mf-field">
                    <label for="sending-ses-access-key">Access key ID</label>
                    <input
                      id="sending-ses-access-key"
                      class="mf-input"
                      name="sesAccessKey"
                      autocomplete="new-password"
                      data-testid="sending-ses-access-key"
                      [(ngModel)]="sesAccessKeyId"
                      required
                    />
                  </div>
                  <div class="mf-field">
                    <label for="sending-ses-secret-key">Secret access key</label>
                    <input
                      id="sending-ses-secret-key"
                      class="mf-input"
                      type="password"
                      name="sesSecretKey"
                      autocomplete="new-password"
                      data-testid="sending-ses-secret-key"
                      [(ngModel)]="sesSecretAccessKey"
                      required
                    />
                  </div>
                </div>
              }
              <div class="field-grid ses-settings">
                <div class="mf-field">
                  <label for="sending-ses-region">AWS region</label>
                  <input
                    id="sending-ses-region"
                    class="mf-input"
                    name="sesRegion"
                    placeholder="us-east-1"
                    data-testid="sending-ses-region"
                    [(ngModel)]="sesRegion"
                    required
                  />
                </div>
                <div class="mf-field">
                  <label for="sending-ses-config">Configuration set <span>(optional)</span></label>
                  <input
                    id="sending-ses-config"
                    class="mf-input"
                    name="sesConfigurationSet"
                    data-testid="sending-ses-configuration-set"
                    [(ngModel)]="sesConfigurationSet"
                  />
                </div>
                <div class="mf-field full">
                  <label for="sending-sns-topic">SNS topic ARN <span>(optional)</span></label>
                  <input
                    id="sending-sns-topic"
                    class="mf-input"
                    name="snsTopicArn"
                    data-testid="sending-sns-topic-arn"
                    [(ngModel)]="snsTopicArn"
                  />
                </div>
              </div>
            </section>
          }

          <div class="mf-field">
            <label for="sending-postal-address">Postal address <span>(recommended)</span></label>
            <textarea
              id="sending-postal-address"
              class="mf-textarea"
              name="postalAddress"
              rows="3"
              data-testid="sending-postal-address"
              [(ngModel)]="postalAddress"
            ></textarea>
          </div>
          @if (!postalAddress.trim()) {
            <div class="postal-warning" role="status" data-testid="sending-postal-warning">
              Add a physical postal address before sending campaigns. Many jurisdictions and
              providers require one in commercial email footers.
            </div>
          }

          @if (profile()?.verify_error; as verifyError) {
            <p class="mf-err" data-testid="sending-verify-error">{{ verifyError }}</p>
          }
          @if (error()) {
            <p class="mf-err" data-testid="sending-profile-error">{{ error() }}</p>
          }

          <div class="actions">
            <button
              type="submit"
              class="mf-btn mf-btn-primary"
              data-testid="sending-profile-save"
              [disabled]="!canSave() || saving()"
            >
              {{ saving() ? 'Saving…' : 'Save profile' }}
            </button>
            <button
              type="button"
              class="mf-btn mf-btn-ghost"
              data-testid="sending-profile-verify"
              [disabled]="!profile() || saving() || verifying()"
              (click)="verify()"
            >
              {{ verifying() ? 'Verifying…' : 'Verify' }}
            </button>
          </div>
        </form>

        @if (profile()) {
          <section class="test-card" data-testid="sending-profile-test-card">
            <div>
              <h2>Send a test</h2>
              <p class="mf-hint">Confirm the provider can deliver before launching a campaign.</p>
            </div>
            <div class="test-row">
              <input
                class="mf-input"
                type="email"
                aria-label="Test recipient"
                placeholder="you@example.com"
                data-testid="sending-test-email"
                [(ngModel)]="testEmail"
              />
              <button
                type="button"
                class="mf-btn mf-btn-ghost"
                data-testid="sending-profile-test"
                [disabled]="profile()?.status !== 'verified' || !testEmail.trim() || testing()"
                (click)="sendTest()"
              >
                {{ testing() ? 'Sending…' : 'Send test' }}
              </button>
            </div>
          </section>
        }
      }
    </div>
  `,
  styles: [
    `
      .page,
      .profile-form,
      .provider-card,
      .test-card {
        display: grid;
        gap: 18px;
      }
      .business-field {
        max-width: 420px;
        margin-bottom: 18px;
      }
      .loading,
      .actions,
      .stored-row,
      .test-row,
      .mode-option {
        display: flex;
        align-items: center;
        gap: 10px;
      }
      .mode-fieldset {
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius);
        display: grid;
        grid-template-columns: repeat(3, minmax(0, 1fr));
        gap: 12px;
        padding: 14px;
      }
      .mode-fieldset legend,
      h2 {
        font-weight: 660;
      }
      .mode-option {
        align-items: flex-start;
        padding: 12px;
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
      }
      .mode-option span {
        display: grid;
        gap: 3px;
      }
      .mode-option small,
      label span {
        color: var(--mf-text-muted);
        font-weight: 400;
      }
      .field-grid {
        display: grid;
        grid-template-columns: repeat(2, minmax(0, 1fr));
        gap: 14px;
      }
      .full {
        grid-column: 1 / -1;
      }
      .provider-card,
      .test-card {
        padding: 18px;
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius);
      }
      .provider-card h2,
      .test-card h2,
      .test-card p {
        margin: 0;
      }
      .ses-settings {
        margin-top: 4px;
      }
      .stored-row {
        flex-wrap: wrap;
      }
      .stored-row .mf-btn {
        margin-left: auto;
      }
      .postal-warning {
        padding: 12px 14px;
        border-left: 4px solid var(--mf-warn);
        background: var(--mf-warn-soft);
        color: var(--mf-warn-text);
        border-radius: var(--mf-radius-sm);
        font-size: var(--mf-fs-sm);
      }
      .test-card {
        grid-template-columns: minmax(0, 1fr) minmax(280px, 1fr);
        align-items: end;
      }
      .test-row .mf-input {
        flex: 1;
      }
      @media (max-width: 800px) {
        .mode-fieldset,
        .field-grid,
        .test-card {
          grid-template-columns: 1fr;
        }
      }
    `,
  ],
})
export class MailingSendingProfileComponent implements OnInit {
  private businessesApi = inject(BusinessService);
  private mailing = inject(MailingService);
  private tickets = inject(TicketService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  businesses = signal<Business[]>([]);
  businessId = signal('');
  profile = signal<MailingSendingProfile | null>(null);
  domains = signal<EmailDomain[]>([]);
  loading = signal(false);
  saving = signal(false);
  verifying = signal(false);
  testing = signal(false);
  replaceCredentials = signal(false);
  error = signal('');

  mode: MailingSendingMode = 'relay';
  fromEmail = '';
  fromName = '';
  replyTo = '';
  postalAddress = '';
  emailDomainId = '';
  resendApiKey = '';
  resendWebhookSecret = '';
  sesAccessKeyId = '';
  sesSecretAccessKey = '';
  sesRegion = '';
  sesConfigurationSet = '';
  snsTopicArn = '';
  testEmail = '';

  profileTone = mailingProfileStatusTone;

  ngOnInit(): void {
    this.businessesApi.list().subscribe({
      next: (page) => {
        const businesses = page.items ?? [];
        this.businesses.set(businesses);
        const businessId = this.current.businessId() ?? businesses[0]?.id;
        if (businessId) this.selectBusiness(businessId);
      },
      error: () => this.error.set('Could not load businesses'),
    });
  }

  selectBusiness(businessId: string): void {
    this.businessId.set(businessId);
    this.current.set(businessId);
    this.profile.set(null);
    this.domains.set([]);
    this.resetForm();
    this.loading.set(true);
    this.error.set('');
    this.loadProfile(businessId);
    this.loadDomains(businessId);
  }

  verifiedDomains(): EmailDomain[] {
    return this.domains().filter((domain) => domain.verification === 'verified');
  }

  credentialsStoredForMode(): boolean {
    const profile = this.profile();
    return !!profile?.has_credentials && profile.mode === this.mode && !this.replaceCredentials();
  }

  modeChanged(): void {
    this.replaceCredentials.set(false);
  }

  canSave(): boolean {
    if (!this.fromEmail.trim() || !this.fromName.trim()) return false;
    if (this.mode === 'relay') return !!this.emailDomainId;
    if (this.mode === 'resend') {
      return this.credentialsStoredForMode() || !!this.resendApiKey.trim();
    }
    return (
      !!this.sesRegion.trim() &&
      (this.credentialsStoredForMode() ||
        (!!this.sesAccessKeyId.trim() && !!this.sesSecretAccessKey.trim()))
    );
  }

  save(): void {
    if (!this.canSave() || this.saving()) return;
    const body: MailingSendingProfileInput = {
      mode: this.mode,
      from_email: this.fromEmail.trim(),
      from_name: this.fromName.trim(),
      reply_to: this.replyTo.trim() || null,
      postal_address: this.postalAddress.trim() || null,
    };
    if (this.mode === 'relay') body.email_domain_id = this.emailDomainId;
    if (this.mode === 'resend' && !this.credentialsStoredForMode()) {
      body.resend = {
        api_key: this.resendApiKey.trim(),
        ...(this.resendWebhookSecret.trim()
          ? { webhook_secret: this.resendWebhookSecret.trim() }
          : {}),
      };
    }
    if (this.mode === 'ses') {
      body.ses_region = this.sesRegion.trim();
      body.ses_configuration_set = this.sesConfigurationSet.trim() || null;
      body.sns_topic_arn = this.snsTopicArn.trim() || null;
      if (!this.credentialsStoredForMode()) {
        body.ses = {
          access_key_id: this.sesAccessKeyId.trim(),
          secret_access_key: this.sesSecretAccessKey.trim(),
        };
      }
    }

    this.saving.set(true);
    this.error.set('');
    this.mailing.putSendingProfile(this.businessId(), body).subscribe({
      next: (profile) => {
        this.saving.set(false);
        this.applyProfile(profile);
        this.toast.success('Sending profile saved');
      },
      error: (error: HttpErrorResponse) => {
        this.saving.set(false);
        this.error.set(this.describeError(error, 'Could not save the sending profile'));
      },
    });
  }

  verify(): void {
    if (!this.profile() || this.verifying()) return;
    this.verifying.set(true);
    this.error.set('');
    this.mailing.verifySendingProfile(this.businessId()).subscribe({
      next: (profile) => {
        this.verifying.set(false);
        this.applyProfile(profile);
        this.toast.success(
          profile.status === 'verified' ? 'Sending profile verified' : 'Verification checked',
        );
      },
      error: (error: HttpErrorResponse) => {
        this.verifying.set(false);
        this.error.set(this.describeError(error, 'Could not verify the sending profile'));
      },
    });
  }

  sendTest(): void {
    const to = this.testEmail.trim();
    if (!to || this.testing() || this.profile()?.status !== 'verified') return;
    this.testing.set(true);
    this.mailing.testSendingProfile(this.businessId(), to).subscribe({
      next: () => {
        this.testing.set(false);
        this.toast.success('Test message sent');
      },
      error: (error: HttpErrorResponse) => {
        this.testing.set(false);
        this.error.set(this.describeError(error, 'Could not send the test message'));
      },
    });
  }

  statusLabel(status: string): string {
    return status === 'unverified' ? 'Unverified' : status === 'verified' ? 'Verified' : 'Error';
  }

  private loadProfile(businessId: string): void {
    this.mailing.getSendingProfile(businessId).subscribe({
      next: (profile) => {
        if (businessId !== this.businessId()) return;
        this.loading.set(false);
        this.applyProfile(profile);
      },
      error: (error: HttpErrorResponse) => {
        if (businessId !== this.businessId()) return;
        this.loading.set(false);
        if (error.status !== 404) {
          this.error.set(this.describeError(error, 'Could not load the sending profile'));
        }
      },
    });
  }

  private loadDomains(businessId: string, cursor?: string, accumulated: EmailDomain[] = []): void {
    this.tickets.listEmailDomains(businessId, cursor).subscribe({
      next: (page) => {
        if (businessId !== this.businessId()) return;
        const domains = [...accumulated, ...(page.items ?? [])];
        if (page.next_cursor) this.loadDomains(businessId, page.next_cursor, domains);
        else this.domains.set(domains);
      },
      error: () => {
        if (businessId === this.businessId()) this.domains.set(accumulated);
      },
    });
  }

  private applyProfile(profile: MailingSendingProfile): void {
    this.profile.set(profile);
    this.mode = profile.mode;
    this.fromEmail = profile.from_email;
    this.fromName = profile.from_name;
    this.replyTo = profile.reply_to ?? '';
    this.postalAddress = profile.postal_address ?? '';
    this.emailDomainId = profile.email_domain_id ?? '';
    this.sesRegion = profile.ses_region ?? '';
    this.sesConfigurationSet = profile.ses_configuration_set ?? '';
    this.snsTopicArn = profile.sns_topic_arn ?? '';
    this.resendApiKey = '';
    this.resendWebhookSecret = '';
    this.sesAccessKeyId = '';
    this.sesSecretAccessKey = '';
    this.replaceCredentials.set(false);
  }

  private resetForm(): void {
    this.mode = 'relay';
    this.fromEmail = '';
    this.fromName = '';
    this.replyTo = '';
    this.postalAddress = '';
    this.emailDomainId = '';
    this.resendApiKey = '';
    this.resendWebhookSecret = '';
    this.sesAccessKeyId = '';
    this.sesSecretAccessKey = '';
    this.sesRegion = '';
    this.sesConfigurationSet = '';
    this.snsTopicArn = '';
    this.replaceCredentials.set(false);
  }

  private describeError(error: HttpErrorResponse, fallback: string): string {
    if (error.status === 400) {
      return (
        (error.error as { message?: string } | null)?.message ||
        'Check the profile fields and try again.'
      );
    }
    if (error.status === 403 || error.status === 404) return "You don't have access to do that.";
    return fallback;
  }
}
