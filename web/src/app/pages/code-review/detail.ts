import { DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute } from '@angular/router';
import {
  CodeReview,
  CodeReviewService,
  Finding,
} from '../../core/code-review.service';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { StatusPill } from '../../ui/status-pill/status-pill';
import { runStatusTone } from '../../ui/status';

// Code Review detail page (/code-review/:businessId/:id). Reads businessId + id from
// the route, calls getReview, and renders the status pill, summary, full findings
// table, and a "View on GitHub" link when review_url is populated. Polls getReview
// every 3 s while the review is non-terminal (pending/running); stops on terminal or
// ngOnDestroy to prevent leaks.
@Component({
  selector: 'app-code-review-detail',
  standalone: true,
  imports: [DatePipe, PageHeader, Spinner, StatusPill],
  template: `
    <div class="mf-card" data-testid="code-review-detail">
      <mf-page-header title="Code Review Detail">
        @if (loading()) {
          <span class="mf-loading-row" data-testid="detail-loading" actions><mf-spinner /></span>
        }
      </mf-page-header>

      @if (error()) {
        <p class="mf-err" data-testid="detail-error">{{ error() }}</p>
      }

      @if (review(); as r) {
        <!-- Header: status + GitHub link -->
        <div style="display:flex;align-items:center;gap:12px;margin-bottom:16px;flex-wrap:wrap">
          <mf-status-pill [tone]="reviewTone(r.status)" [label]="r.status" />
          <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">PR #{{ r.pr_number }}</span>
          <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ r.created_at | date:'short' }}</span>
          @if (r.review_url) {
            <a [href]="r.review_url" target="_blank" rel="noopener noreferrer"
               class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="view-on-github">
              View on GitHub
            </a>
          }
        </div>

        <!-- Live progress (running only) -->
        @if (r.status === 'running' && r.progress) {
          <div class="mf-card" style="margin-bottom:16px" data-testid="review-progress">
            <div style="display:flex;gap:12px;align-items:center;margin-bottom:8px">
              <span style="font-weight:600" data-testid="progress-phase">{{ phaseLabel(r) }}</span>
              <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" data-testid="progress-elapsed">{{ elapsedLabel() }}</span>
              @if (r.progress.tokens) {
                <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ r.progress.tokens }} tokens</span>
              }
            </div>
            @if (r.progress.preview) {
              <pre data-testid="progress-preview"
                   style="max-height:240px;overflow:auto;white-space:pre-wrap;font-family:monospace;font-size:var(--mf-fs-xs);margin:0;padding:8px;border-radius:6px;background:var(--mf-bg-subtle,rgba(0,0,0,.05))">{{ r.progress.preview }}</pre>
            }
          </div>
        }

        <!-- Failed reviews keep their last streamed output so the failure is diagnosable
             instead of blank. The detailed error stays server-side (it can carry provider/
             sandbox internals); the preview shown here is already secret-redacted. -->
        @if (r.status === 'failed') {
          <div class="mf-card" style="margin-bottom:16px" data-testid="review-failed">
            <p style="margin:0 0 8px;font-weight:600;color:var(--mf-danger,#c0392b)">This review failed.</p>
            @if (r.progress?.preview) {
              <p style="margin:0 0 6px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">Output captured before it failed:</p>
              <pre data-testid="review-failed-output"
                   style="max-height:240px;overflow:auto;white-space:pre-wrap;font-family:monospace;font-size:var(--mf-fs-xs);margin:0;padding:8px;border-radius:6px;background:var(--mf-bg-subtle,rgba(0,0,0,.05))">{{ r.progress?.preview }}</pre>
            } @else {
              <p style="margin:0;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" data-testid="review-failed-nooutput">No output was captured before the failure.</p>
            }
          </div>
        }

        <!-- Summary -->
        @if (r.summary) {
          <div class="mf-card" style="margin-bottom:16px" data-testid="review-summary">
            <p style="margin:0;white-space:pre-wrap">{{ r.summary }}</p>
          </div>
        }

        <!-- Findings table -->
        <h3 style="margin:0 0 8px;font-size:var(--mf-fs-sm);font-weight:600;color:var(--mf-text-muted);text-transform:uppercase;letter-spacing:.05em">
          Findings ({{ r.findings_count }})
        </h3>
        <div class="mf-table" data-testid="findings-table">
          <div class="mf-tr mf-th">
            <span style="flex:2">File</span>
            <span style="width:60px">Line</span>
            <span style="width:80px">Severity</span>
            <span style="flex:2">Title</span>
            <span style="flex:3">Detail</span>
          </div>
          @for (f of r.findings; track $index) {
            <div class="mf-tr" data-testid="finding-row">
              <span style="flex:2;font-size:var(--mf-fs-sm);font-family:monospace;word-break:break-all">{{ f.file }}</span>
              <span style="width:60px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ f.line ?? '—' }}</span>
              <span style="width:80px;font-size:var(--mf-fs-sm)">
                <mf-status-pill [tone]="findingTone(f.severity)" [label]="f.severity" />
              </span>
              <span style="flex:2;font-size:var(--mf-fs-sm);font-weight:500">{{ f.title }}</span>
              <span style="flex:3;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ f.detail }}</span>
            </div>
          }
          @if (!r.findings.length) {
            <div class="mf-tr" style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" data-testid="findings-empty">
              No findings.
            </div>
          }
        </div>
      }
    </div>
  `,
})
export class CodeReviewDetailComponent implements OnInit, OnDestroy {
  private route = inject(ActivatedRoute);
  private api = inject(CodeReviewService);

  readonly reviewTone = runStatusTone;

  review = signal<CodeReview | null>(null);
  loading = signal(true);
  error = signal('');
  elapsed = signal(0);

  private businessId = '';
  private id = '';
  private pollTimer: ReturnType<typeof setInterval> | undefined;
  private elapsedTimer: ReturnType<typeof setInterval> | undefined;

  // Maps Finding severity to a StatusPill tone.
  findingTone(severity: Finding['severity']): 'danger' | 'warn' | 'neutral' {
    switch (severity) {
      case 'error': return 'danger';
      case 'warning': return 'warn';
      default: return 'neutral';
    }
  }

  // Maps a progress phase to a human label (reviewing names the model).
  phaseLabel(r: CodeReview): string {
    const phase = r.progress?.phase ?? 'working';
    const map: Record<string, string> = {
      preparing: 'Preparing',
      reviewing: 'Reviewing with ' + (r.model || 'model'),
      posting: 'Posting review',
    };
    return map[phase] ?? phase;
  }

  // Formats the running-review elapsed seconds as "Ns" or "Mm Ns".
  elapsedLabel(): string {
    const s = this.elapsed();
    const m = Math.floor(s / 60);
    const sec = s % 60;
    return m > 0 ? `${m}m ${sec}s` : `${sec}s`;
  }

  private startElapsed(createdAt: string): void {
    if (this.elapsedTimer !== undefined) return;
    const start = new Date(createdAt).getTime();
    const tick = () => this.elapsed.set(Math.max(0, Math.floor((Date.now() - start) / 1000)));
    tick();
    this.elapsedTimer = setInterval(tick, 1000);
  }

  private stopElapsed(): void {
    if (this.elapsedTimer !== undefined) {
      clearInterval(this.elapsedTimer);
      this.elapsedTimer = undefined;
    }
  }

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.id = this.route.snapshot.paramMap.get('id') ?? '';
    this.load();
  }

  ngOnDestroy(): void {
    this.stopPolling();
  }

  private isTerminal(r: CodeReview): boolean {
    return r.status === 'succeeded' || r.status === 'failed';
  }

  private startPolling(): void {
    if (this.pollTimer !== undefined) return;
    this.pollTimer = setInterval(() => this.pollOnce(), 3000);
  }

  private stopPolling(): void {
    if (this.pollTimer !== undefined) {
      clearInterval(this.pollTimer);
      this.pollTimer = undefined;
    }
    this.stopElapsed();
  }

  private pollOnce(): void {
    if (!this.businessId || !this.id) return;
    this.api.getReview(this.businessId, this.id).subscribe({
      next: (r) => {
        this.review.set(r);
        if (this.isTerminal(r)) {
          this.stopPolling();
        } else {
          this.startElapsed(r.created_at);
        }
      },
      error: () => {
        // Keep polling on transient errors.
      },
    });
  }

  load(): void {
    if (!this.businessId || !this.id) {
      this.loading.set(false);
      this.error.set('Could not load this review.');
      return;
    }
    this.loading.set(true);
    this.error.set('');
    this.api.getReview(this.businessId, this.id).subscribe({
      next: (r) => {
        this.review.set(r);
        this.loading.set(false);
        if (!this.isTerminal(r)) {
          this.startPolling();
          this.startElapsed(r.created_at);
        }
      },
      error: (e: HttpErrorResponse) => {
        this.loading.set(false);
        this.error.set(
          e.status === 403 || e.status === 404
            ? "You don't have access to this review."
            : 'Could not load this review.',
        );
      },
    });
  }
}
