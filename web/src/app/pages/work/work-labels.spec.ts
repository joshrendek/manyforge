import { describe, expect, it } from 'vitest';
import { makeTask } from './work-fixtures';
import {
  byClosedDesc,
  byPriority,
  isOpen,
  priorityTone,
  sameList,
  workStatusLabel,
  workStatusTone,
} from './work-labels';

describe('work labels', () => {
  it('labels and tones statuses', () => {
    expect(workStatusLabel('in_progress')).toBe('In progress');
    expect(workStatusLabel('mystery')).toBe('mystery');
    expect(workStatusTone('blocked')).toBe('danger');
    expect(workStatusTone('done')).toBe('success');
  });

  it('tones priorities from critical down', () => {
    expect([0, 1, 2, 3, 4].map(priorityTone)).toEqual([
      'danger',
      'warn',
      'neutral',
      'neutral',
      'neutral',
    ]);
  });

  it('orders by priority, then oldest first', () => {
    const a = makeTask({ key: 'a', priority: 2, created_at: '2026-10-02T00:00:00Z' });
    const b = makeTask({ key: 'b', priority: 0, created_at: '2026-10-03T00:00:00Z' });
    const c = makeTask({ key: 'c', priority: 2, created_at: '2026-10-01T00:00:00Z' });
    expect([a, b, c].sort(byPriority).map((t) => t.key)).toEqual(['b', 'c', 'a']);
  });

  it('orders closed tasks most recent first', () => {
    const a = makeTask({ key: 'a', closed_at: '2026-10-01T00:00:00Z' });
    const b = makeTask({ key: 'b', closed_at: '2026-10-05T00:00:00Z' });
    expect([a, b].sort(byClosedDesc).map((t) => t.key)).toEqual(['b', 'a']);
  });

  it('treats done and canceled as closed', () => {
    expect(isOpen(makeTask({ status: 'blocked' }))).toBe(true);
    expect(isOpen(makeTask({ status: 'done' }))).toBe(false);
    expect(isOpen(makeTask({ status: 'canceled' }))).toBe(false);
  });

  it('compares lists by order and content', () => {
    expect(sameList(['a', 'b'], ['a', 'b'])).toBe(true);
    expect(sameList(['a', 'b'], ['b', 'a'])).toBe(false);
    expect(sameList([], ['a'])).toBe(false);
  });
});
