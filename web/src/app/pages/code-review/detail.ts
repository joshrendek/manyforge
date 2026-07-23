import { DatePipe } from '@angular/common';
import { HttpErrorResponse } from '@angular/common/http';
import { Component, OnDestroy, OnInit, inject, signal } from '@angular/core';
import { ActivatedRoute } from '@angular/router';
import {
  CodeReview,
  CodeReviewService,
  DimensionRun,
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

        <!-- Findings -->
        <h3 style="margin:0 0 8px;font-size:var(--mf-fs-sm);font-weight:600;color:var(--mf-text-muted);text-transform:uppercase;letter-spacing:.05em">
          Findings ({{ r.findings_count }})
        </h3>

        @if (isGrouped()) {
          <!-- Multi-dimension review: one findings table per lane, headed by its
               dimension + finding count (spec 008). -->
          @for (g of dimensionGroups(); track g.dimension) {
            <div data-testid="dimension-group" style="margin-bottom:16px">
              <h4 data-testid="dimension-group-header"
                  style="display:flex;align-items:center;gap:8px;margin:0 0 6px;font-size:var(--mf-fs-base);font-weight:600">
                <span style="text-transform:capitalize">{{ g.dimension }}</span>
                <mf-status-pill tone="neutral" [label]="g.findings.length + ''" [ariaLabel]="g.findings.length + ' findings'" />
                @if (g.model) {
                  <span data-testid="dimension-model" [title]="g.model"
                        style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm);font-weight:400;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">{{ g.model }}</span>
                }
              </h4>
              <div class="mf-table" role="table" [attr.aria-label]="g.dimension + ' findings'">
                <div class="mf-tr mf-th" role="row">
                  <span style="flex:2" role="columnheader">File</span>
                  <span style="width:60px" role="columnheader">Line</span>
                  <span style="width:80px" role="columnheader">Severity</span>
                  <span style="flex:2" role="columnheader">Title</span>
                  <span style="flex:3" role="columnheader">Detail</span>
                </div>
                @for (f of g.findings; track $index) {
                  <div class="mf-tr" data-testid="finding-row" role="row">
                    <span style="flex:2;font-size:var(--mf-fs-sm);font-family:monospace;word-break:break-all" role="cell">{{ f.file }}</span>
                    <span style="width:60px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" role="cell">{{ f.line ?? '—' }}</span>
                    <span style="width:80px;font-size:var(--mf-fs-sm)" role="cell">
                      <mf-status-pill [tone]="findingTone(f.severity)" [label]="f.severity" [ariaLabel]="'severity ' + f.severity" />
                    </span>
                    <span style="flex:2;font-size:var(--mf-fs-sm);font-weight:500" role="cell">{{ f.title }}@if (f.rule_id) {<span data-testid="finding-rule" style="margin-left:6px;padding:1px 6px;border-radius:4px;background:var(--mf-surface-2);color:var(--mf-text-muted);font-size:var(--mf-fs-xs);font-weight:400">rule: {{ f.rule_id }}</span>}</span>
                    <span style="flex:3;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" role="cell">{{ f.detail }}</span>
                  </div>
                }
              </div>
            </div>
          }
        } @else {
          <div class="mf-table" data-testid="findings-table" role="table" aria-label="Findings">
            <div class="mf-tr mf-th" role="row">
              <span style="flex:2" role="columnheader">File</span>
              <span style="width:60px" role="columnheader">Line</span>
              <span style="width:80px" role="columnheader">Severity</span>
              <span style="flex:2" role="columnheader">Title</span>
              <span style="flex:3" role="columnheader">Detail</span>
            </div>
            @for (f of r.findings; track $index) {
              <div class="mf-tr" data-testid="finding-row" role="row">
                <span style="flex:2;font-size:var(--mf-fs-sm);font-family:monospace;word-break:break-all" role="cell">{{ f.file }}</span>
                <span style="width:60px;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" role="cell">{{ f.line ?? '—' }}</span>
                <span style="width:80px;font-size:var(--mf-fs-sm)" role="cell">
                  <mf-status-pill [tone]="findingTone(f.severity)" [label]="f.severity" [ariaLabel]="'severity ' + f.severity" />
                </span>
                <span style="flex:2;font-size:var(--mf-fs-sm);font-weight:500" role="cell">{{ f.title }}@if (f.rule_id) {<span data-testid="finding-rule" style="margin-left:6px;padding:1px 6px;border-radius:4px;background:var(--mf-surface-2);color:var(--mf-text-muted);font-size:var(--mf-fs-xs);font-weight:400">rule: {{ f.rule_id }}</span>}</span>
                <span style="flex:3;color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" role="cell">{{ f.detail }}</span>
              </div>
            }
            @if (!r.findings.length) {
              <div class="mf-tr" style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)" data-testid="findings-empty" role="row">
                <span role="cell">No findings.</span>
              </div>
            }
          </div>
        }

        <!-- Configured lanes that did not run this review — surfaced, never silently
             dropped (spec 008 FR-003). -->
        @if (skippedDimensions().length) {
          <div class="mf-card" data-testid="skipped-dimensions" style="margin-top:16px">
            <h3 style="margin:0 0 8px;font-size:var(--mf-fs-sm);font-weight:600;color:var(--mf-text-muted);text-transform:uppercase;letter-spacing:.05em">
              Skipped dimensions
            </h3>
            <ul style="list-style:none;margin:0;padding:0">
              @for (s of skippedDimensions(); track s.dimension) {
                <li data-testid="skipped-dimension-row" style="display:flex;gap:8px;align-items:center">
                  <span style="font-weight:500;text-transform:capitalize">{{ s.dimension }}</span>
                  <span style="color:var(--mf-text-muted);font-size:var(--mf-fs-sm)">{{ s.skipped_reason || 'skipped' }}</span>
                </li>
              }
            </ul>
          </div>
        }
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

  // A review is "grouped" once any finding carries a dimension tag (spec 008). Legacy
  // single-lane reviews leave every finding untagged, so they render the flat table
  // unchanged — this is the load-bearing back-compat trigger.
  isGrouped(): boolean {
    return !!this.review()?.findings.some((f) => (f.dimension ?? '').trim() !== '');
  }

  // Groups the review's findings by dimension, preserving first-seen order. Untagged
  // findings (e.g. a general lane mixed with specialists) fall under "general".
  dimensionGroups(): { dimension: string; findings: Finding[]; model?: string }[] {
    const r = this.review();
    if (!r) return [];
    // The per-lane model lives in dimension_runs (the top-level model is the "panel"
    // sentinel for a multi-dimension review) — surface it per group.
    const modelByDim = new Map<string, string>();
    for (const dr of r.dimension_runs ?? []) {
      if (dr.model) modelByDim.set(dr.dimension, dr.model);
    }
    const order: string[] = [];
    const byDim = new Map<string, Finding[]>();
    for (const f of r.findings) {
      const key = (f.dimension ?? '').trim() || 'general';
      if (!byDim.has(key)) {
        byDim.set(key, []);
        order.push(key);
      }
      byDim.get(key)!.push(f);
    }
    return order.map((d) => ({ dimension: d, findings: byDim.get(d)!, model: modelByDim.get(d) }));
  }

  // Configured lanes that did not run this review (scoped out, disabled, etc.). Surfaced
  // so a skipped dimension is never silently absent (spec 008 FR-003).
  skippedDimensions(): DimensionRun[] {
    return (this.review()?.dimension_runs ?? []).filter((d) => d.status === 'skipped');
  }

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
