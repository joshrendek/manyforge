import { HttpErrorResponse } from '@angular/common/http';
import { WorkLane, WorkStatus, WorkTask, WorkType } from '../../core/work.service';
import { Tone } from '../../ui/status';

// Shared vocabulary for the Work Board pages: labels, tones, ordering, and the dialog focus trap.

export const ACTIVE_COLUMNS: { status: WorkStatus; label: string }[] = [
  { status: 'todo', label: 'To do' },
  { status: 'in_progress', label: 'In progress' },
  { status: 'blocked', label: 'Blocked' },
  { status: 'done', label: 'Done' },
];

export const STATUSES: WorkStatus[] = [
  'backlog',
  'todo',
  'in_progress',
  'blocked',
  'done',
  'canceled',
];
export const LANES: WorkLane[] = ['agent', 'operator'];
export const TYPES: WorkType[] = ['task', 'bug', 'feature', 'chore'];
export const PRIORITIES = [0, 1, 2, 3, 4];

const STATUS_LABELS: Record<WorkStatus, string> = {
  backlog: 'Backlog',
  todo: 'To do',
  in_progress: 'In progress',
  blocked: 'Blocked',
  done: 'Done',
  canceled: 'Canceled',
};

export function workStatusLabel(s: string): string {
  return STATUS_LABELS[s as WorkStatus] ?? s;
}

export function workStatusTone(s: string): Tone {
  switch (s) {
    case 'in_progress':
      return 'accent';
    case 'blocked':
      return 'danger';
    case 'done':
      return 'success';
    case 'todo':
      return 'warn';
    default:
      return 'neutral';
  }
}

const PRIORITY_HINTS = ['critical', 'high', 'normal', 'low', 'backlog'];

export function priorityLabel(p: number): string {
  return `P${p}`;
}

export function priorityTitle(p: number): string {
  return `P${p} · ${PRIORITY_HINTS[p] ?? 'unknown'}`;
}

export function priorityTone(p: number): Tone {
  if (p <= 0) return 'danger';
  if (p === 1) return 'warn';
  return 'neutral';
}

export function laneLabel(l: string): string {
  return l === 'operator' ? 'Operator' : 'Agent';
}

export function isOpen(t: WorkTask): boolean {
  return t.status !== 'done' && t.status !== 'canceled';
}

// Priority first (P0 on top), then oldest first so long-waiting asks don't sink.
export function byPriority(a: WorkTask, b: WorkTask): number {
  return a.priority - b.priority || a.created_at.localeCompare(b.created_at);
}

// Most recently closed first.
export function byClosedDesc(a: WorkTask, b: WorkTask): number {
  return (b.closed_at ?? b.updated_at).localeCompare(a.closed_at ?? a.updated_at);
}

export function sameList(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

// Keeps Tab / Shift+Tab inside a modal card (mirrors the enrollments dialog).
export function trapTab(event: KeyboardEvent, card: HTMLElement | undefined): void {
  if (event.key !== 'Tab' || !card) return;
  const focusables = Array.from(
    card.querySelectorAll<HTMLElement>(
      'button, input, select, textarea, [href], [tabindex]:not([tabindex="-1"])',
    ),
  ).filter((el) => !el.hasAttribute('disabled'));
  if (!focusables.length) return;
  const first = focusables[0];
  const last = focusables[focusables.length - 1];
  const active = document.activeElement;
  if (event.shiftKey && (active === first || active === card)) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && active === last) {
    event.preventDefault();
    first.focus();
  }
}

export function describeWorkMutation(e: HttpErrorResponse, fallback: string): string {
  if (e.status === 400) {
    const msg = (e.error as { message?: string } | null)?.message;
    return msg ? `That change was rejected: ${msg}` : 'That change was rejected. Check your input.';
  }
  if (e.status === 409) return 'That conflicts with the current state.';
  if (e.status === 403 || e.status === 404) return "You don't have access to do that.";
  return fallback;
}
