import { NodeStats } from '../../../../core/automations.service';

// A node is "completed" once its step reaches a terminal, non-error outcome:
// it advanced to the next node, sent its email, took a branch, or exited.
// `entered`, `waiting`, and `errors` are still-open outcomes and are excluded.
export function nodeCompletedCount(stats: NodeStats): number {
  return stats.advanced + stats.sent + stats.branch_yes + stats.branch_no + stats.exited;
}
