import { WorkProject, WorkTask } from '../../core/work.service';

// Builders shared by the Work Board unit specs.

export function makeProject(over: Partial<WorkProject> = {}): WorkProject {
  return {
    id: 'p1',
    slug: 'manyforge',
    name: 'ManyForge',
    key_prefix: 'mf',
    created_at: '2026-10-01T00:00:00Z',
    counts: { backlog: 1, todo: 2, in_progress: 1, blocked: 0, operator_open: 1 },
    ...over,
  };
}

export function makeTask(over: Partial<WorkTask> = {}): WorkTask {
  return {
    key: 'mf-aaaaaa',
    title: 'A task',
    description: '',
    notes: '',
    lane: 'agent',
    status: 'todo',
    priority: 2,
    type: 'task',
    assignee: '',
    labels: [],
    blocked_by: [],
    rev: 1,
    created_at: '2026-10-10T00:00:00Z',
    updated_at: '2026-10-10T00:00:00Z',
    closed_at: null,
    ...over,
  };
}
