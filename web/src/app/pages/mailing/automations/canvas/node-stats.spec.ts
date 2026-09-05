import { describe, expect, it } from 'vitest';
import { NodeStats } from '../../../../core/automations.service';
import { nodeCompletedCount } from './node-stats';

function stats(partial: Partial<NodeStats> = {}): NodeStats {
  return {
    node_id: 'n1',
    node_kind: 'send_email',
    entered: 0,
    waiting: 0,
    advanced: 0,
    sent: 0,
    opened: 0,
    clicked: 0,
    branch_yes: 0,
    branch_no: 0,
    exited: 0,
    errors: 0,
    ...partial,
  };
}

describe('nodeCompletedCount', () => {
  it('counts terminal, non-error outcomes only', () => {
    expect(nodeCompletedCount(stats())).toBe(0);
    expect(
      nodeCompletedCount(
        stats({ advanced: 3, sent: 2, branch_yes: 1, branch_no: 1, exited: 1 }),
      ),
    ).toBe(8);
  });

  it('excludes still-open and error outcomes', () => {
    expect(
      nodeCompletedCount(stats({ entered: 5, waiting: 4, errors: 2, sent: 1 })),
    ).toBe(1);
  });
});
