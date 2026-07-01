import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CodeReview, Finding } from '../../core/code-review.service';
import { CodeReviewDetailComponent } from './detail';

const biz = 'b1';
const reviewId = 'r1';

function makeFinding(over: Partial<Finding> = {}): Finding {
  return {
    file: 'src/main.ts',
    line: 42,
    severity: 'warning',
    title: 'Unused variable',
    detail: 'The variable `x` is declared but never used.',
    ...over,
  };
}

function makeReview(over: Partial<CodeReview> = {}): CodeReview {
  return {
    id: reviewId,
    status: 'succeeded',
    summary: 'Looks good overall, minor warnings.',
    review_url: 'https://github.com/acme/api/pull/1#pullrequestreview-1',
    pr_number: 42,
    model: 'google/gemini-2.5-pro',
    findings: [makeFinding()],
    findings_count: 1,
    cost_cents: 0,
    created_at: '2026-06-20T12:00:00Z',
    posted_at: '2026-06-20T12:01:00Z',
    ...over,
  };
}

describe('CodeReviewDetailComponent', () => {
  let fixture: ComponentFixture<CodeReviewDetailComponent>;
  let cmp: CodeReviewDetailComponent;
  let mock: HttpTestingController;

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }

  function qAll(sel: string): NodeListOf<HTMLElement> {
    return fixture.nativeElement.querySelectorAll(sel) as NodeListOf<HTMLElement>;
  }

  beforeEach(() => {
    vi.useFakeTimers();
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        {
          provide: ActivatedRoute,
          useValue: {
            snapshot: {
              paramMap: new Map([
                ['businessId', biz],
                ['id', reviewId],
              ]),
            },
          },
        },
      ],
    });
    mock = TestBed.inject(HttpTestingController);
  });

  afterEach(() => {
    vi.useRealTimers();
    mock.verify();
    document.documentElement.setAttribute('data-theme', 'light');
    localStorage.clear();
  });

  /** Mount and flush the initial getReview call. */
  function mount(review: CodeReview = makeReview()): void {
    fixture = TestBed.createComponent(CodeReviewDetailComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges();
    mock.expectOne(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`).flush(review);
    fixture.detectChanges();
  }

  // ── Rendering ────────────────────────────────────────────────────────────────

  it('renders the review summary', () => {
    mount(makeReview({ summary: 'Looks good overall, minor warnings.' }));
    const summary = q('[data-testid="review-summary"]');
    expect(summary).toBeTruthy();
    expect(summary?.textContent).toContain('Looks good overall, minor warnings.');
  });

  it('renders one finding-row per finding', () => {
    mount(makeReview({
      findings: [
        makeFinding({ file: 'src/a.ts', title: 'Issue A' }),
        makeFinding({ file: 'src/b.ts', title: 'Issue B' }),
      ],
      findings_count: 2,
    }));
    const rows = qAll('[data-testid="finding-row"]');
    expect(rows.length).toBe(2);
    const text = fixture.nativeElement.textContent as string;
    expect(text).toContain('src/a.ts');
    expect(text).toContain('Issue A');
    expect(text).toContain('src/b.ts');
    expect(text).toContain('Issue B');
  });

  it('renders finding file, line, severity, title, and detail', () => {
    mount(makeReview({
      findings: [
        makeFinding({ file: 'lib/auth.ts', line: 99, severity: 'error', title: 'SQL injection', detail: 'Use parameterized queries.' }),
      ],
      findings_count: 1,
    }));
    const row = q('[data-testid="finding-row"]') as HTMLElement;
    expect(row).toBeTruthy();
    const text = row.textContent ?? '';
    expect(text).toContain('lib/auth.ts');
    expect(text).toContain('99');
    expect(text).toContain('SQL injection');
    expect(text).toContain('Use parameterized queries.');
  });

  it('renders a null line as em-dash', () => {
    mount(makeReview({ findings: [makeFinding({ line: null })], findings_count: 1 }));
    const row = q('[data-testid="finding-row"]') as HTMLElement;
    expect(row.textContent).toContain('—');
  });

  it('renders a view-on-github link when review_url is present', () => {
    mount(makeReview({ review_url: 'https://github.com/acme/api/pull/1#pullrequestreview-1' }));
    const link = q('[data-testid="view-on-github"]') as HTMLAnchorElement;
    expect(link).toBeTruthy();
    expect(link.href).toContain('github.com');
    expect(link.textContent?.trim()).toBe('View on GitHub');
  });

  it('does NOT render view-on-github when review_url is empty', () => {
    mount(makeReview({ review_url: '' }));
    expect(q('[data-testid="view-on-github"]')).toBeNull();
  });

  it('renders the status pill with the review status', () => {
    mount(makeReview({ status: 'succeeded' }));
    // mf-status-pill renders a .mf-pill element; the text should contain the status.
    const detail = q('[data-testid="code-review-detail"]') as HTMLElement;
    expect(detail.textContent).toContain('succeeded');
  });

  it('shows no findings empty state when findings array is empty', () => {
    mount(makeReview({ findings: [], findings_count: 0 }));
    expect(q('[data-testid="findings-empty"]')).toBeTruthy();
    expect(qAll('[data-testid="finding-row"]').length).toBe(0);
  });

  // ── Live progress ──────────────────────────────────────────────────────────────

  it('renders the progress block (phase + preview) while running', () => {
    mount(makeReview({ status: 'running', progress: { phase: 'reviewing', tokens: 12, preview: 'partial output here' } }));
    expect(q('[data-testid="review-progress"]')).toBeTruthy();
    expect(q('[data-testid="progress-phase"]')?.textContent).toContain('Reviewing');
    expect(q('[data-testid="progress-preview"]')?.textContent).toContain('partial output here');
    // drain the poll the running status scheduled (so mock.verify() is clean)
    vi.advanceTimersByTime(3000);
    mock.expectOne(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`).flush(makeReview({ status: 'succeeded' }));
  });

  it('hides the progress block when terminal', () => {
    mount(makeReview({ status: 'succeeded', progress: { phase: 'posting', tokens: 1, preview: 'x' } }));
    expect(q('[data-testid="review-progress"]')).toBeNull();
  });

  it('ticks the elapsed timer while running', () => {
    mount(makeReview({ status: 'running', progress: { phase: 'reviewing', tokens: 0, preview: '' } }));
    const before = cmp.elapsed();
    vi.advanceTimersByTime(3000);
    expect(cmp.elapsed()).toBeGreaterThan(before);
    // the 3s advance also fired a poll — drain it
    mock.expectOne(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`).flush(makeReview({ status: 'succeeded' }));
  });

  // ── Polling while non-terminal ────────────────────────────────────────────────

  it('polls getReview while review is pending', () => {
    mount(makeReview({ status: 'pending' }));
    // After mount, review is pending — polling should be active.
    // Advance the timer by 3 s to trigger the first poll.
    vi.advanceTimersByTime(3000);
    // Flush the poll request with still-pending status.
    mock.expectOne(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`).flush(
      makeReview({ status: 'pending' }),
    );
    fixture.detectChanges();
    expect(cmp.review()?.status).toBe('pending');
  });

  it('stops polling once review transitions to succeeded', () => {
    mount(makeReview({ status: 'pending' }));

    // First poll tick — review is still pending.
    vi.advanceTimersByTime(3000);
    mock.expectOne(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`).flush(
      makeReview({ status: 'pending' }),
    );
    fixture.detectChanges();

    // Second poll tick — review is now succeeded.
    vi.advanceTimersByTime(3000);
    mock.expectOne(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`).flush(
      makeReview({ status: 'succeeded' }),
    );
    fixture.detectChanges();
    expect(cmp.review()?.status).toBe('succeeded');

    // Advance time further — no more HTTP requests should be made.
    vi.advanceTimersByTime(9000);
    mock.expectNone(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`);
  });

  it('does NOT poll when review is already terminal on load', () => {
    mount(makeReview({ status: 'failed' }));
    // Advance timer — no poll requests should fire.
    vi.advanceTimersByTime(6000);
    mock.expectNone(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`);
  });

  it('polls while running and stops on succeeded', () => {
    mount(makeReview({ status: 'running' }));

    vi.advanceTimersByTime(3000);
    mock.expectOne(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`).flush(
      makeReview({ status: 'succeeded' }),
    );
    fixture.detectChanges();

    vi.advanceTimersByTime(6000);
    mock.expectNone(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`);
  });

  it('cleans up the poll timer on ngOnDestroy', () => {
    mount(makeReview({ status: 'pending' }));
    fixture.destroy();
    // After destroy, timer is cleared — advancing should produce no requests.
    vi.advanceTimersByTime(9000);
    mock.expectNone(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`);
  });

  // ── Error handling ────────────────────────────────────────────────────────────

  it('shows a generic error on load failure', () => {
    fixture = TestBed.createComponent(CodeReviewDetailComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges();
    mock.expectOne(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`).flush(
      {}, { status: 500, statusText: 'Server Error' },
    );
    fixture.detectChanges();
    expect(q('[data-testid="detail-error"]')).toBeTruthy();
    expect(q('[data-testid="detail-error"]')?.textContent).toContain('Could not load');
  });

  it('shows access error on 404', () => {
    fixture = TestBed.createComponent(CodeReviewDetailComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges();
    mock.expectOne(`/api/v1/businesses/${biz}/code-reviews/${reviewId}`).flush(
      {}, { status: 404, statusText: 'Not Found' },
    );
    fixture.detectChanges();
    const errEl = q('[data-testid="detail-error"]');
    expect(errEl).toBeTruthy();
    expect(errEl?.textContent).toContain("don't have access");
  });

  // ── Dark theme ───────────────────────────────────────────────────────────────

  it('renders in dark theme', () => {
    document.documentElement.setAttribute('data-theme', 'dark');
    mount();
    expect(q('.mf-card')).toBeTruthy();
  });
});
