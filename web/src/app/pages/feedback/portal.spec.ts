import { provideHttpClient } from '@angular/common/http';
import {
  HttpTestingController,
  provideHttpClientTesting,
  TestRequest,
} from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { PublicPost } from '../../core/public-feedback.service';
import { FeedbackPortalComponent } from './portal';

const key = 'fbk_demo';

function makePost(over: Partial<PublicPost> = {}): PublicPost {
  return {
    id: 'p1',
    title: 'Dark mode',
    body: 'Please',
    status: 'open',
    vote_count: 4,
    created_at: '',
    viewer_voted: false,
    identity_verified: false,
    ...over,
  };
}

describe('FeedbackPortalComponent', () => {
  let fixture: ComponentFixture<FeedbackPortalComponent>;
  let cmp: FeedbackPortalComponent;
  let mock: HttpTestingController;

  // Matches the list GET regardless of its query string — voter_identity is a per-test random
  // device id (see ensureDeviceId), so it can't be hard-coded into the expected URL.
  function expectListReq(): TestRequest {
    return mock.expectOne(
      (r) => r.method === 'GET' && r.url === `/api/v1/feedback/public/${key}/posts`,
    );
  }

  function load(posts: PublicPost[] = [makePost()]): void {
    fixture = TestBed.createComponent(FeedbackPortalComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges(); // ngOnInit → listPosts
    const req = expectListReq();
    // voter_identity is the portal's stable, localStorage-persisted device id — not a secret.
    expect(req.request.params.get('voter_identity')).toBe(localStorage.getItem('mf_fb_device'));
    expect(req.request.params.get('voter_identity')).toBeTruthy();
    req.flush({ items: posts });
    fixture.detectChanges();
  }

  function q(sel: string): HTMLElement | null {
    return fixture.nativeElement.querySelector(sel) as HTMLElement | null;
  }

  beforeEach(() => {
    localStorage.clear();
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(),
        provideHttpClientTesting(),
        provideRouter([]),
        { provide: ActivatedRoute, useValue: { snapshot: { paramMap: new Map([['key', key]]) } } },
      ],
    });
    mock = TestBed.inject(HttpTestingController);
    document.documentElement.setAttribute('data-theme', 'light');
  });
  afterEach(() => {
    localStorage.clear();
    document.documentElement.setAttribute('data-theme', 'light');
  });

  it('renders public posts with an upvote button (no auth)', () => {
    load();
    expect(q('[data-testid="portal-post"]')).toBeTruthy();
    expect(q('[data-testid="portal-post-title"]')?.textContent).toContain('Dark mode');
    expect(q('[data-testid="portal-vote-count"]')?.textContent).toContain('4');
  });

  it('shows the unavailable state on a 401 (unknown/revoked key or private board)', () => {
    fixture = TestBed.createComponent(FeedbackPortalComponent);
    cmp = fixture.componentInstance;
    fixture.detectChanges();
    expectListReq().flush(
      { code: 'UNAUTHORIZED', message: 'unauthorized' },
      { status: 401, statusText: 'Unauthorized' },
    );
    fixture.detectChanges();
    expect(q('[data-testid="portal-unavailable"]')).toBeTruthy();
    expect(cmp.unavailable()).toBe(true);
  });

  it('submits a new idea with an anonymous author identity then reloads', () => {
    load();
    cmp.title = 'Add SSO';
    cmp.submit();
    const req = mock.expectOne(`/api/v1/feedback/public/${key}/posts`);
    expect(req.request.method).toBe('POST');
    expect(req.request.body.title).toBe('Add SSO');
    expect(typeof req.request.body.author_identity).toBe('string');
    expect(req.request.body.author_identity.length).toBeGreaterThan(0);
    req.flush({ id: 'p2', title: 'Add SSO', status: 'open', vote_count: 0 });
    expectListReq().flush({
      items: [makePost(), makePost({ id: 'p2', title: 'Add SSO', vote_count: 0 })],
    });
    fixture.detectChanges();
    expect(cmp.title).toBe('');
  });

  it('upvotes a post with the device identity and reflects the new count', () => {
    load();
    cmp.upvote(makePost());
    const req = mock.expectOne(`/api/v1/feedback/public/${key}/posts/p1/votes`);
    expect(req.request.method).toBe('POST');
    expect(typeof req.request.body.voter_identity).toBe('string');
    req.flush({ voted: true, vote_count: 5 });
    fixture.detectChanges();
    expect(cmp.posts()[0].vote_count).toBe(5);
    expect(cmp.voted().has('p1')).toBe(true);
  });

  it('sends the persisted device id as voter_identity on the list call', () => {
    load();
    const deviceId = localStorage.getItem('mf_fb_device');
    expect(deviceId).toBeTruthy();
    expect(cmp.posts().length).toBe(1);
  });

  it('reflects viewer_voted from the server as "voted" on load, before any local vote', () => {
    // A fresh browser (no local vote cache) that reloads a board where the server already
    // knows this device voted (viewer_voted: true) must render as voted from server truth
    // alone, not from an optimistic local click.
    load([makePost({ viewer_voted: true }), makePost({ id: 'p2', viewer_voted: false })]);
    expect(cmp.voted().has('p1')).toBe(true);
    expect(cmp.voted().has('p2')).toBe(false);
    const votedButtons = fixture.nativeElement.querySelectorAll('[data-testid="portal-upvote"]');
    expect((votedButtons[0] as HTMLElement).classList.contains('voted')).toBe(true);
    expect((votedButtons[1] as HTMLElement).classList.contains('voted')).toBe(false);
  });

  it('keeps viewer_voted=true across a reload even after localStorage is cleared', () => {
    load([makePost({ viewer_voted: true })]);
    expect(cmp.voted().has('p1')).toBe(true);
    // Simulate a cleared vote cache (e.g. different device/browser profile) — the same
    // device id is still sent, and the server still says viewer_voted: true.
    localStorage.removeItem(`mf_fb_voted_${key}`);
    cmp.load();
    expectListReq().flush({ items: [makePost({ viewer_voted: true })] });
    fixture.detectChanges();
    expect(cmp.voted().has('p1')).toBe(true);
    expect(q('[data-testid="portal-upvote"]')?.classList.contains('voted')).toBe(true);
  });

  it('does not render voted when the local cache is stale and the server says viewer_voted:false', () => {
    // Seed the local optimistic cache as if a prior vote succeeded (or a stale entry lingered),
    // then have the server report viewer_voted:false for that post on load — server truth must
    // win and the button must not render as voted.
    localStorage.setItem(`mf_fb_voted_${key}`, JSON.stringify(['p1']));
    load([makePost({ viewer_voted: false })]);
    expect(cmp.voted().has('p1')).toBe(false);
    expect(q('[data-testid="portal-upvote"]')?.classList.contains('voted')).toBe(false);
  });

  it('shows a verified badge when the server reports identity_verified', () => {
    load([makePost({ identity_verified: true })]);
    expect(q('[data-testid="portal-verified-badge"]')).toBeTruthy();
  });

  it('omits the verified badge when identity_verified is false', () => {
    load([makePost({ identity_verified: false })]);
    expect(q('[data-testid="portal-verified-badge"]')).toBeFalsy();
  });
});
