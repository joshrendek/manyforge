import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { Subject, takeUntil } from 'rxjs';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal, DestroyRef, effect, untracked } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { DatePipe } from '@angular/common';
import { RouterLink } from '@angular/router';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import {
  MailingSendingMode,
  MailingSendingProfile,
  MailingSendingProfileInput,
  MailingService,
  MailingSetup,
  MailingSetupCheck,
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
  imports: [FormsModule, PageHeader, Spinner, StatusPill, DatePipe, RouterLink],
  template: `
    <div class="mf-card page" data-testid="mailing-sending-profile-page">
      <mf-page-header
        [eyebrow]="businessName()"
        title="Sending setup"
        subtitle="Connect a provider, verify your sender and feedback, then send a test."
      />
      @if (businessId()) {
        <nav class="steps" aria-label="Sending setup stages">
          @for (label of stages; track $index) {
            <button
              type="button"
              class="mf-btn"
              [class.mf-btn-primary]="stage() === $index"
              [class.mf-btn-ghost]="stage() !== $index"
              [attr.aria-current]="stage() === $index ? 'step' : null"
              (click)="stage.set($index)"
            >
              {{ $index + 1 }}. {{ label }}
            </button>
          }
        </nav>
        @if (loading()) {
          <p class="loading" data-testid="sending-profile-loading">
            <mf-spinner /> Loading saved setup…
          </p>
        }
        @if (instanceBlocker()) {
          <div class="postal-warning" role="status" data-testid="sending-instance-blocker">
            {{ instanceBlocker() }} Configuration is available below; verification and test delivery
            are paused.
            <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" (click)="stage.set(0)">
              View administrator actions
            </button>
          </div>
        }
        <section class="provider-card" [hidden]="stage() !== 0" aria-labelledby="instance-heading">
          <h2 id="instance-heading">1. Instance readiness</h2>
          <p class="mf-hint">
            Checks apply to
            {{
              mode === 'relay' ? 'ManyForge relay' : mode === 'resend' ? 'Resend' : 'Amazon SES'
            }}. Only an administrator can change deployment settings. Choose a provider in the next
            stage to update the requirements.
          </p>
          @if (setupError()) {
            <p class="mf-err" role="alert">{{ setupError() }}</p>
          }
          @for (check of applicableChecks(); track check.id) {
            <div class="readiness-check">
              <div class="stored-row">
                <b>{{ check.label }}</b>
                <mf-status-pill
                  [tone]="check.status === 'ready' ? 'success' : 'warn'"
                  [label]="check.status === 'ready' ? 'Ready' : 'Administrator action required'"
                />
              </div>
              <p>{{ check.message }}</p>
              @if (check.status === 'blocked') {
                <p><b>Next action:</b> {{ check.action }}</p>
              }
            </div>
          }
          <button
            type="button"
            class="mf-btn mf-btn-ghost"
            [disabled]="setupLoading() || busy()"
            data-testid="sending-setup-refresh"
            (click)="refreshSetup()"
          >
            {{ setupLoading() ? 'Checking…' : 'Refresh checks' }}
          </button>
        </section>
        <form
          class="profile-form"
          data-testid="sending-profile-form"
          (ngSubmit)="save()"
          (input)="testAccepted.set(false)"
          (change)="testAccepted.set(false)"
        >
          <fieldset class="configuration" [disabled]="busy() || loading()">
            <section
              class="profile-form"
              [hidden]="stage() !== 1"
              aria-labelledby="provider-heading"
            >
              <h2 id="provider-heading">2. Provider credentials</h2>
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
              @if (mode === 'relay') {
                <p class="mf-hint">
                  No provider credentials are needed here. Your administrator maintains the SMTP
                  relay; connect a verified domain in the next stage.
                </p>
              }
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
                      <label for="sending-ses-config">Configuration set</label>
                      <input
                        id="sending-ses-config"
                        class="mf-input"
                        name="sesConfigurationSet"
                        data-testid="sending-ses-configuration-set"
                        [(ngModel)]="sesConfigurationSet"
                      />
                    </div>
                    <div class="mf-field full">
                      <label for="sending-sns-topic">SNS topic ARN</label>
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
              @if (mode === 'resend') {
                <p class="mf-hint">
                  Use a Resend API key with permission to send mail, inspect domains, and manage
                  webhooks. Verification creates the feedback webhook automatically; no webhook
                  secret is needed.
                </p>
              } @else if (mode === 'ses') {
                <p class="mf-hint">
                  Use credentials for the selected SES region with permission to send mail and
                  inspect sender identities and configuration sets. Configure feedback in stage 4
                  before verification.
                </p>
              }
            </section>
            <section
              class="profile-form"
              [hidden]="stage() !== 2"
              aria-labelledby="identity-heading"
            >
              <h2 id="identity-heading">3. Sender and DNS identity</h2>
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
              <div class="mf-field">
                <label for="sending-postal-address"
                  >Postal address <span>(recommended)</span></label
                >
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
              @if (mode === 'relay') {
                <div class="stored-row">
                  <a
                    class="mf-btn mf-btn-ghost"
                    [routerLink]="['/support', businessId(), 'settings', 'inbox']"
                    >Manage email domains in Support settings</a
                  >
                  <button
                    type="button"
                    class="mf-btn mf-btn-ghost"
                    [disabled]="domainsLoading() || !!checkingDomain()"
                    (click)="refreshDomains()"
                  >
                    {{ domainsLoading() ? 'Refreshing…' : 'Refresh domains' }}
                  </button>
                </div>
                @if (domainError()) {
                  <p class="mf-err" role="alert">{{ domainError() }}</p>
                }
                <p class="mf-hint">
                  Publish these records at your DNS provider, then check DNS. Records below are
                  instructions to publish, not evidence they are live. Only a verified domain can be
                  selected above.
                </p>
                @for (domain of domains(); track domain.id) {
                  <details class="provider-card">
                    <summary>
                      {{ domain.domain }} — {{ domain.verification }} · DKIM:
                      {{ domain.dkim_state }} · SPF: {{ domain.spf_state }}
                    </summary>
                    <dl class="dns-records">
                      <dt>Ownership TXT · {{ domain.dns_challenge.verification_txt.name }}</dt>
                      <dd>
                        <code>{{ domain.dns_challenge.verification_txt.value }}</code>
                      </dd>
                      <dt>DKIM TXT · {{ domain.dns_challenge.dkim_record.name }}</dt>
                      <dd>
                        <code>{{ domain.dns_challenge.dkim_record.value }}</code>
                      </dd>
                      <dt>SPF TXT · {{ domain.domain }}</dt>
                      <dd>
                        <code>{{ domain.dns_challenge.spf_hint }}</code>
                      </dd>
                      @if (domain.dns_challenge.mx_hint) {
                        <dt>MX guidance</dt>
                        <dd>{{ domain.dns_challenge.mx_hint }}</dd>
                      }
                    </dl>
                    <p class="mf-hint">
                      Last domain verification:
                      {{
                        domain.verified_at
                          ? (domain.verified_at | date: 'medium')
                          : 'Not yet verified'
                      }}
                    </p>
                    <button
                      type="button"
                      class="mf-btn mf-btn-ghost"
                      [disabled]="!!checkingDomain() || domainsLoading()"
                      (click)="checkDomain(domain.id)"
                    >
                      {{ checkingDomain() === domain.id ? 'Checking DNS…' : 'Check DNS' }}
                    </button>
                  </details>
                }
              } @else if (mode === 'resend') {
                <p class="mf-hint">
                  Add the From email domain in Resend, publish the DNS records shown there, and wait
                  for its domain verification. Use an address on that verified domain.
                </p>
                <a href="https://resend.com/domains" target="_blank" rel="noopener noreferrer"
                  >Open Resend domains</a
                >
              } @else {
                <p class="mf-hint">
                  In the SES console for your selected region, verify the From identity and publish
                  its DKIM records. If your account is in the SES sandbox, verify the test recipient
                  too or request production access.
                </p>
                <a
                  href="https://console.aws.amazon.com/ses/"
                  target="_blank"
                  rel="noopener noreferrer"
                  >Open Amazon SES</a
                >
              }
            </section>
          </fieldset>
          <section
            class="provider-card"
            [hidden]="stage() !== 3"
            aria-labelledby="feedback-heading"
          >
            <h2 id="feedback-heading">4. Verify provider and feedback</h2>
            @if (mode === 'resend') {
              <p>
                Verification checks your sender domain and automatically creates a webhook for
                delivery, bounce, and complaint events. Allow the API key to manage webhooks and
                make sure this instance’s public URL is reachable.
              </p>
            } @else if (mode === 'ses') {
              <p>
                In SES, create a configuration set in your sending region with an enabled SNS event
                destination for Bounce and Complaint. Delivery events are also recommended. Enter
                that configuration set and its SNS topic ARN in stage 2.
              </p>
              <p>
                Subscribe the SNS topic to this instance’s HTTPS feedback endpoint below, allow SNS
                to reach it, and wait for subscription confirmation. Re-run verification after
                publishing the configuration. The topic and configuration set must match.
              </p>
              @if (profile(); as saved) {
                <code class="feedback-path"
                  >{{ applicationOrigin }}/api/v1/inbound/mailing/{{ saved.id }}/ses</code
                >
              } @else {
                <p class="mf-hint">
                  Save the profile first to obtain its unique feedback endpoint.
                </p>
              }
              <p class="mf-hint">
                This suggested URL uses the current application origin. Confirm that it matches your
                administrator-configured public URL before subscribing. A configured topic alone is
                not confirmed feedback readiness.
              </p>
            } @else {
              <p>
                Verification checks the relay and selected sending domain. Relay feedback is managed
                by the instance; resolve any administrator checks before verifying.
              </p>
            }
            @if (profile(); as saved) {
              <div class="readiness-check" data-testid="sending-provider-readiness">
                <div class="stored-row">
                  <b>Saved provider verification</b>
                  <mf-status-pill
                    [tone]="profileTone(saved.status)"
                    [label]="statusLabel(saved.status)"
                    data-testid="sending-profile-status"
                  />
                </div>
                <p class="mf-hint">
                  Last verified:
                  {{
                    saved.last_verified_at
                      ? (saved.last_verified_at | date: 'medium')
                      : 'Not yet verified'
                  }}
                </p>
                @if (saved.verify_error) {
                  <p class="mf-err" data-testid="sending-verify-error">{{ saved.verify_error }}</p>
                }
              </div>
              <div class="readiness-check" data-testid="sending-feedback-readiness">
                <div class="stored-row">
                  <b>Saved feedback verification</b>
                  <mf-status-pill
                    [tone]="
                      saved.feedback_status === 'ready'
                        ? 'success'
                        : saved.feedback_status === 'error'
                          ? 'danger'
                          : 'warn'
                    "
                    [label]="
                      saved.feedback_status === 'ready'
                        ? 'Ready'
                        : saved.feedback_status === 'error'
                          ? 'Error'
                          : 'Pending verification'
                    "
                  />
                </div>
                <p class="mf-hint">
                  Feedback confirmed:
                  {{
                    saved.feedback_confirmed_at
                      ? (saved.feedback_confirmed_at | date: 'medium')
                      : 'Not yet confirmed'
                  }}
                </p>
                @if (saved.feedback_error) {
                  <p class="mf-err">{{ saved.feedback_error }}</p>
                }
              </div>
            }
            @if (verificationBlocker()) {
              <p class="postal-warning">{{ verificationBlocker() }}</p>
            }
            <button
              type="button"
              class="mf-btn mf-btn-ghost"
              data-testid="sending-profile-verify"
              [disabled]="!!verificationBlocker() || busy()"
              (click)="verify()"
            >
              {{ verifying() ? 'Verifying…' : 'Verify provider and feedback' }}
            </button>
          </section>
          @if (hasUnsavedChanges()) {
            <p class="postal-warning" data-testid="sending-unsaved">
              You have unsaved changes. Save the profile before verifying or sending a test.
            </p>
          }
          @if (!mailingKeyReady()) {
            <p class="mf-hint">
              Saving requires an available mailing encryption key. Ask an administrator to complete
              the instance checks, then refresh checks.
            </p>
          }
          @if (error()) {
            <p class="mf-err" role="alert" data-testid="sending-profile-error">{{ error() }}</p>
          }
          @if (stage() === 1 || stage() === 2 || hasUnsavedChanges()) {
            <div class="actions">
              <button
                type="submit"
                class="mf-btn mf-btn-primary"
                data-testid="sending-profile-save"
                [disabled]="!canSave() || !hasUnsavedChanges() || busy()"
              >
                {{ saving() ? 'Saving…' : 'Save profile' }}
              </button>
            </div>
          }
        </form>
        <section
          class="provider-card"
          [hidden]="stage() !== 4"
          data-testid="sending-profile-test-card"
          aria-labelledby="test-heading"
        >
          <h2 id="test-heading">5. Test delivery</h2>
          <p class="mf-hint">
            Send one test to an address you control. Provider acceptance is not proof of inbox
            delivery: check the recipient’s inbox and spam folder.
          </p>
          @if (testBlocker()) {
            <p class="postal-warning" data-testid="sending-test-blocker">{{ testBlocker() }}</p>
          }
          <div class="test-row">
            <input
              class="mf-input"
              type="email"
              aria-label="Test recipient"
              placeholder="you@example.com"
              data-testid="sending-test-email"
              [(ngModel)]="testEmail"
              [disabled]="busy()"
            />
            <button
              type="button"
              class="mf-btn mf-btn-ghost"
              data-testid="sending-profile-test"
              [disabled]="!!testBlocker() || !testEmail.trim() || busy()"
              (click)="sendTest()"
            >
              {{ testing() ? 'Sending…' : 'Send test' }}
            </button>
          </div>
          @if (testAccepted() && !hasUnsavedChanges()) {
            <p role="status" data-testid="sending-test-success">
              The provider accepted the test message. Check the recipient’s inbox and spam folder to
              confirm delivery.
            </p>
          }
        </section>
        <div class="actions stage-navigation">
          <button
            type="button"
            class="mf-btn mf-btn-ghost"
            [disabled]="stage() === 0"
            (click)="stage.set(stage() - 1)"
          >
            Back
          </button>
          <span class="mf-hint">Stage {{ stage() + 1 }} of {{ stages.length }}</span>
          <button
            type="button"
            class="mf-btn mf-btn-primary"
            [disabled]="stage() === stages.length - 1"
            (click)="stage.set(stage() + 1)"
          >
            Continue
          </button>
        </div>
      }
    </div>
  `,
  styles: [
    `
      [hidden] {
        display: none !important;
      }
      .configuration {
        border: 0;
        padding: 0;
        margin: 0;
        min-width: 0;
      }
      .steps {
        display: flex;
        flex-wrap: wrap;
        gap: 8px;
      }
      .steps .mf-btn {
        flex: 1 1 150px;
        white-space: normal;
      }
      .readiness-check {
        border-bottom: 1px solid var(--mf-border);
        padding-bottom: 12px;
      }
      .readiness-check p {
        margin: 8px 0 0;
      }
      .dns-records {
        margin: 0;
      }
      .dns-records dt {
        font-weight: 660;
        margin-top: 12px;
      }
      .dns-records dd {
        margin: 4px 0 0;
        overflow-wrap: anywhere;
      }
      .feedback-path {
        overflow-wrap: anywhere;
      }
      .stage-navigation {
        justify-content: space-between;
      }
      summary {
        cursor: pointer;
      }
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
  private readonly destroyRef = inject(DestroyRef);
  private readonly businessChanged = new Subject<void>();
  private readonly followBusiness = effect(() => {
    const id = this.current.businessId() ?? '';
    untracked(() => {
      if (id !== this.businessId()) this.selectBusiness(id);
    });
  });
  businessName(): string {
    return this.businesses().find((business) => business.id === this.businessId())?.name ?? '';
  }

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
  readonly stages = ['Instance', 'Provider', 'Sender & DNS', 'Feedback', 'Test delivery'];
  readonly applicationOrigin = typeof location !== 'undefined' ? location.origin : '';
  stage = signal(0);
  setup = signal<MailingSetup | null>(null);
  setupLoading = signal(false);
  setupError = signal('');
  domainsLoading = signal(false);
  domainError = signal('');
  checkingDomain = signal('');
  testAccepted = signal(false);
  private profileLoaded = false;

  mode: MailingSendingMode = 'relay';
  fromEmail = '';
  fromName = '';
  replyTo = '';
  postalAddress = '';
  emailDomainId = '';
  resendApiKey = '';
  sesAccessKeyId = '';
  sesSecretAccessKey = '';
  sesRegion = '';
  sesConfigurationSet = '';
  snsTopicArn = '';
  testEmail = '';

  profileTone = mailingProfileStatusTone;

  ngOnInit(): void {
    this.businessesApi
      .list()
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
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
    if (businessId === this.businessId()) return;
    this.businessChanged.next();
    this.setup.set(null);
    this.setupLoading.set(false);
    this.setupError.set('');
    this.domainError.set('');
    this.domainsLoading.set(false);
    this.checkingDomain.set('');
    this.testAccepted.set(false);
    this.stage.set(0);
    this.profileLoaded = false;
    this.loading.set(false);
    this.error.set('');
    this.testEmail = '';
    this.saving.set(false);
    this.verifying.set(false);
    this.testing.set(false);
    this.replaceCredentials.set(false);
    this.businessId.set(businessId);
    if (businessId) this.current.set(businessId);
    this.profile.set(null);
    this.domains.set([]);
    this.resetForm();
    if (!businessId) return;
    this.loading.set(true);
    this.error.set('');
    this.refreshSetup();
    this.refreshDomains();
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
    this.testAccepted.set(false);
    this.resendApiKey = '';
    this.sesAccessKeyId = '';
    this.sesSecretAccessKey = '';
  }

  busy(): boolean {
    return this.saving() || this.verifying() || this.testing();
  }

  applicableChecks(): MailingSetupCheck[] {
    return this.setup()?.checks.filter((check) => check.required_for.includes(this.mode)) ?? [];
  }

  mailingKeyReady(): boolean {
    return (
      this.setup()?.checks.some(
        (check) => check.id === 'mailing_key' && check.status === 'ready',
      ) ?? false
    );
  }

  instanceBlocker(): string {
    if (this.setupLoading()) return 'Instance readiness checks are running.';
    if (!this.setup() || !this.applicableChecks().length)
      return 'Instance readiness is unknown. Refresh checks in stage 1.';
    const blocked = this.applicableChecks().filter((check) => check.status !== 'ready');
    return blocked.length
      ? `Administrator action required: ${blocked.map((check) => check.label).join(', ')}. See stage 1 for instructions.`
      : '';
  }

  hasUnsavedChanges(): boolean {
    const saved = this.profile();
    if (!saved)
      return !!(
        this.fromName.trim() ||
        this.fromEmail.trim() ||
        this.replyTo.trim() ||
        this.postalAddress.trim() ||
        this.emailDomainId ||
        this.resendApiKey ||
        this.sesAccessKeyId ||
        this.sesSecretAccessKey ||
        this.sesRegion.trim() ||
        this.sesConfigurationSet.trim() ||
        this.snsTopicArn.trim() ||
        this.mode !== 'relay'
      );
    return (
      this.mode !== saved.mode ||
      this.fromName.trim() !== saved.from_name ||
      this.fromEmail.trim() !== saved.from_email ||
      this.replyTo.trim() !== (saved.reply_to ?? '') ||
      this.postalAddress.trim() !== (saved.postal_address ?? '') ||
      this.replaceCredentials() ||
      (this.mode === 'relay' && this.emailDomainId !== (saved.email_domain_id ?? '')) ||
      (this.mode === 'resend' && !!this.resendApiKey) ||
      (this.mode === 'ses' &&
        (!!this.sesAccessKeyId ||
          !!this.sesSecretAccessKey ||
          this.sesRegion.trim() !== (saved.ses_region ?? '') ||
          this.sesConfigurationSet.trim() !== (saved.ses_configuration_set ?? '') ||
          this.snsTopicArn.trim() !== (saved.sns_topic_arn ?? '')))
    );
  }

  verificationBlocker(): string {
    if (!this.profile()) return 'Save a sending profile using stages 2 and 3 before verification.';
    if (this.hasUnsavedChanges())
      return 'Save your changes before verifying. Verification uses the saved profile.';
    if (this.instanceBlocker()) return this.instanceBlocker();
    if (this.mode === 'ses' && (!this.sesConfigurationSet.trim() || !this.snsTopicArn.trim())) {
      return 'Enter the SES configuration set and SNS topic ARN in stage 2, then save before verifying feedback.';
    }
    return '';
  }

  testBlocker(): string {
    if (this.verificationBlocker()) return this.verificationBlocker();
    if (this.profile()?.status !== 'verified')
      return 'Verify the provider in stage 4 before sending a test.';
    if (this.profile()?.feedback_status !== 'ready')
      return 'Feedback is not ready. Complete the provider-specific feedback setup and verify again in stage 4.';
    return '';
  }

  refreshSetup(): void {
    const businessId = this.businessId();
    if (!businessId || this.setupLoading() || this.busy()) return;
    this.setupLoading.set(true);
    this.setupError.set('');
    this.mailing
      .getSetup(businessId)
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (setup) => {
          this.setup.set(setup);
          this.setupLoading.set(false);
          if (this.mailingKeyReady() && !this.profileLoaded) {
            this.loading.set(true);
            this.loadProfile(businessId);
          } else {
            this.loading.set(false);
          }
        },
        error: () => {
          this.setup.set(null);
          this.setupLoading.set(false);
          this.loading.set(false);
          this.setupError.set(
            'Could not check instance readiness. Refresh checks to retry; contact an administrator if this continues.',
          );
        },
      });
  }

  refreshDomains(): void {
    if (!this.businessId() || this.domainsLoading() || this.checkingDomain()) return;
    this.domainsLoading.set(true);
    this.domainError.set('');
    this.loadDomains(this.businessId());
  }

  checkDomain(domainId: string): void {
    if (this.checkingDomain()) return;
    this.checkingDomain.set(domainId);
    this.domainError.set('');
    this.tickets
      .verifyEmailDomain(this.businessId(), domainId)
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (domain) => {
          this.domains.update((domains) =>
            domains.map((current) => (current.id === domain.id ? domain : current)),
          );
          this.checkingDomain.set('');
        },
        error: () => {
          this.checkingDomain.set('');
          this.domainError.set(
            'Could not check DNS. Confirm the records at your DNS provider and retry, or manage the domain in Support settings.',
          );
        },
      });
  }

  canSave(): boolean {
    if (!this.mailingKeyReady() || !this.profileLoaded || this.loading()) return false;
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
    if (!this.canSave() || !this.hasUnsavedChanges() || this.busy()) return;
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
    this.mailing
      .putSendingProfile(this.businessId(), body)
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
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
    if (this.verificationBlocker() || this.busy()) return;
    this.verifying.set(true);
    this.error.set('');
    this.mailing
      .verifySendingProfile(this.businessId())
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (profile) => {
          this.verifying.set(false);
          this.applyProfile(profile);
          this.toast.success(
            profile.status === 'verified' && profile.feedback_status === 'ready'
              ? 'Provider and feedback verified'
              : 'Verification completed; review the readiness results',
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
    if (!to || this.busy() || this.testBlocker()) return;
    this.testAccepted.set(false);
    this.error.set('');
    this.testing.set(true);
    this.mailing
      .testSendingProfile(this.businessId(), to)
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: () => {
          this.testing.set(false);
          this.testAccepted.set(true);
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
    this.mailing
      .getSendingProfile(businessId)
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (profile) => {
          if (businessId !== this.businessId()) return;
          this.loading.set(false);
          this.profileLoaded = true;
          this.applyProfile(profile);
        },
        error: (error: HttpErrorResponse) => {
          if (businessId !== this.businessId()) return;
          this.loading.set(false);
          this.profileLoaded = error.status === 404;
          if (error.status !== 404) {
            this.error.set(this.describeError(error, 'Could not load the sending profile'));
          }
        },
      });
  }

  private loadDomains(businessId: string, cursor?: string, accumulated: EmailDomain[] = []): void {
    this.tickets
      .listEmailDomains(businessId, cursor)
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (page) => {
          if (businessId !== this.businessId()) return;
          const domains = [...accumulated, ...(page.items ?? [])];
          if (page.next_cursor) this.loadDomains(businessId, page.next_cursor, domains);
          else {
            this.domains.set(domains);
            this.domainsLoading.set(false);
          }
        },
        error: () => {
          if (businessId === this.businessId()) {
            this.domains.set(accumulated);
            this.domainsLoading.set(false);
            this.domainError.set(
              'Could not load all email domains. Refresh domains to retry or open Support settings.',
            );
          }
        },
      });
  }

  private applyProfile(profile: MailingSendingProfile): void {
    this.testAccepted.set(false);
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
    if (error.status === 403)
      return 'You do not have permission for this action. Ask a business administrator for mailing permissions.';
    if (error.status === 404)
      return 'The sending profile service is unavailable. Refresh instance checks and confirm the mailing key is configured with an administrator.';
    return fallback;
  }
}
