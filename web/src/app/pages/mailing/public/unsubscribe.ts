import { Component, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute } from '@angular/router';
import { PublicMailingService } from '../../../core/public-mailing.service';
import { MailingPublicDoneComponent, MailingPublicShellComponent } from './public-shared';

@Component({
  selector: 'app-mailing-unsubscribe',
  standalone: true,
  imports: [MailingPublicDoneComponent, MailingPublicShellComponent],
  template: `
    <app-mailing-public-shell>
      @if (done()) {
        <app-mailing-public-done />
      } @else {
        <section class="mf-card action-card" data-testid="mailing-unsubscribe-page">
          <h1>Unsubscribe</h1>
          <p class="mf-hint">Stop receiving messages from this mailing list.</p>
          <button
            type="button"
            class="mf-btn mf-btn-primary"
            data-testid="mailing-unsubscribe-submit"
            [disabled]="submitting()"
            (click)="submit()"
          >
            {{ submitting() ? 'Unsubscribing…' : 'Unsubscribe' }}
          </button>
        </section>
      }
    </app-mailing-public-shell>
  `,
  styles: [
    `
      .action-card {
        display: grid;
        gap: 16px;
        text-align: center;
        padding: 32px;
      }
      h1,
      p {
        margin: 0;
      }
    `,
  ],
})
export class MailingUnsubscribeComponent implements OnInit {
  private route = inject(ActivatedRoute);
  private mailing = inject(PublicMailingService);

  token = '';
  submitting = signal(false);
  done = signal(false);

  ngOnInit(): void {
    this.token = this.route.snapshot.paramMap.get('token') ?? '';
  }

  submit(): void {
    if (this.submitting()) return;
    this.submitting.set(true);
    this.mailing
      .unsubscribe(this.token)
      .subscribe({ next: () => this.finish(), error: () => this.finish() });
  }

  private finish(): void {
    this.submitting.set(false);
    this.done.set(true);
  }
}
