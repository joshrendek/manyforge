import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnInit, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { ActivatedRoute } from '@angular/router';
import { PublicMailingService } from '../../../core/public-mailing.service';
import { MailingPublicDoneComponent, MailingPublicShellComponent } from './public-shared';

@Component({
  selector: 'app-mailing-subscribe',
  standalone: true,
  imports: [FormsModule, MailingPublicDoneComponent, MailingPublicShellComponent],
  template: `
    <app-mailing-public-shell>
      @if (done()) {
        <app-mailing-public-done
          heading="Check your inbox"
          message="Open the confirmation link we sent to finish subscribing."
        />
      } @else {
        <section class="mf-card subscribe-card" data-testid="mailing-subscribe-page">
          <h1>Join {{ listName }}</h1>
          <p class="mf-hint">Get updates delivered to your inbox.</p>
          <form (ngSubmit)="submit()" data-testid="mailing-subscribe-form">
            <div class="mf-field">
              <label for="mailing-public-email">Email</label>
              <input
                id="mailing-public-email"
                class="mf-input"
                type="email"
                name="email"
                autocomplete="email"
                data-testid="mailing-public-email"
                [(ngModel)]="email"
                required
              />
            </div>
            <div class="mf-field">
              <label for="mailing-public-first-name">First name <span>(optional)</span></label>
              <input
                id="mailing-public-first-name"
                class="mf-input"
                name="firstName"
                autocomplete="given-name"
                data-testid="mailing-public-first-name"
                [(ngModel)]="firstName"
              />
            </div>
            <div class="honeypot" aria-hidden="true">
              <label for="mailing-public-website">Website</label>
              <input
                id="mailing-public-website"
                name="website"
                tabindex="-1"
                autocomplete="off"
                [(ngModel)]="website"
              />
            </div>
            @if (networkError()) {
              <p class="mf-err" role="alert" data-testid="mailing-public-error">
                We couldn't reach the server. Check your connection and try again.
              </p>
            }
            <button
              type="submit"
              class="mf-btn mf-btn-primary"
              data-testid="mailing-public-submit"
              [disabled]="!email.trim() || submitting()"
            >
              {{ submitting() ? 'Subscribing…' : networkError() ? 'Try again' : 'Subscribe' }}
            </button>
          </form>
        </section>
      }
    </app-mailing-public-shell>
  `,
  styles: [
    `
      .subscribe-card,
      form {
        display: grid;
        gap: 16px;
      }
      h1,
      p {
        margin: 0;
      }
      h1 {
        font-size: var(--mf-fs-xl);
      }
      label span {
        color: var(--mf-text-muted);
        font-weight: 400;
      }
      .honeypot {
        position: absolute;
        left: -10000px;
        width: 1px;
        height: 1px;
        overflow: hidden;
      }
    `,
  ],
})
export class MailingSubscribeComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private mailing = inject(PublicMailingService);

  key = '';
  listName = 'our mailing list';
  email = '';
  firstName = '';
  website = '';
  submitting = signal(false);
  done = signal(false);
  networkError = signal(false);

  ngOnInit(): void {
    this.key = this.route.snapshot.paramMap.get('key') ?? '';
    this.listName = this.route.snapshot.queryParamMap.get('name')?.trim() || 'our mailing list';
    this.done.set(this.route.snapshot.queryParamMap.get('state') === 'check-inbox');
  }

  submit(): void {
    if (!this.key || !this.email.trim() || this.submitting()) return;
    this.submitting.set(true);
    this.networkError.set(false);
    this.mailing
      .subscribe(this.key, {
        email: this.email.trim(),
        first_name: this.firstName.trim() || null,
        website: this.website,
      })
      .subscribe({
        next: () => this.finish(),
        error: (error: HttpErrorResponse) => {
          this.submitting.set(false);
          if (error.status === 0) this.networkError.set(true);
          else this.done.set(true);
        },
      });
  }

  private finish(): void {
    this.submitting.set(false);
    this.done.set(true);
  }
}
