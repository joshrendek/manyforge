import { expect, test, type Page } from '@playwright/test';

// Work Board (spec 018) against a fully mocked management API. A tiny in-memory "server" keeps
// tasks so PATCH/POST round-trips render the way the real backend would.

const profile = {
  id: '1',
  email: 'a@b.c',
  display_name: 'A',
  email_verified: true,
  status: 'active',
};
const biz = {
  items: [
    {
      id: 'b1',
      parent_id: null,
      tenant_root_id: 'b1',
      name: 'Acme',
      status: 'active',
      is_tenant_root: true,
    },
  ],
  next_cursor: null,
};
const project = {
  id: 'p1',
  slug: 'manyforge',
  name: 'ManyForge',
  key_prefix: 'mf',
  created_at: '2026-10-01T00:00:00Z',
  counts: { backlog: 2, todo: 2, in_progress: 1, blocked: 1, operator_open: 2 },
};

type Task = Record<string, unknown> & { key: string; lane: string; status: string };

function task(over: Partial<Task> & { key: string; title: string }): Task {
  return {
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

function seedTasks(): Task[] {
  return [
    task({ key: 'mf-todo01', title: 'Wire the sync endpoint', priority: 1 }),
    task({
      key: 'mf-prog01',
      title: 'Write the mf CLI',
      status: 'in_progress',
      assignee: 'claude',
    }),
    task({ key: 'mf-blck01', title: 'Ship billing', status: 'blocked', blocked_by: ['mf-oper01'] }),
    task({
      key: 'mf-oper01',
      title: 'Rotate the Stripe live key',
      description: 'Dashboard → Developers → API keys → Roll key.\nPaste it into the prod secret.',
      lane: 'operator',
      priority: 0,
    }),
    task({
      key: 'mf-oper02',
      title: 'Approve the DNS TXT record',
      lane: 'operator',
      status: 'backlog',
    }),
    task({ key: 'mf-back01', title: 'Dark-mode polish', status: 'backlog', priority: 3 }),
    task({
      key: 'mf-done01',
      title: 'Bootstrap repo',
      status: 'done',
      closed_at: '2026-10-12T00:00:00Z',
    }),
  ];
}

interface Mock {
  tasks: Task[];
  patches: { key: string; body: Record<string, unknown> }[];
  creates: Record<string, unknown>[];
}

async function install(page: Page, opts: { openOperator?: number } = {}): Promise<Mock> {
  const state: Mock = { tasks: seedTasks(), patches: [], creates: [] };
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  // Defensive fallback first: Playwright matches the most recently added route, so the specific
  // mocks below win, and any other shell call (nav badges) never reaches a real backend.
  await page.route('**/api/**', (r) => r.fulfill({ json: { items: [], next_cursor: null } }));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
  await page.route('**/api/v1/businesses/b1/work/summary', (r) =>
    r.fulfill({
      json: {
        open_operator:
          opts.openOperator ??
          state.tasks.filter(
            (t) => t.lane === 'operator' && !['done', 'canceled'].includes(t.status),
          ).length,
        open_agent: 3,
        in_progress: 1,
      },
    }),
  );
  await page.route('**/api/v1/businesses/b1/work/projects', (r) =>
    r.fulfill({ json: { items: [project] } }),
  );
  await page.route('**/api/v1/businesses/b1/work/projects/p1', (r) => r.fulfill({ json: project }));
  await page.route('**/api/v1/businesses/b1/work/projects/p1/tasks', (r) => {
    if (r.request().method() === 'POST') {
      const body = r.request().postDataJSON() as Record<string, unknown>;
      state.creates.push(body);
      const created = task({ key: 'mf-new001', title: String(body['title']), ...body } as Task);
      state.tasks.push(created);
      return r.fulfill({ status: 201, json: created });
    }
    return r.fulfill({ json: { items: state.tasks } });
  });
  await page.route('**/api/v1/businesses/b1/work/projects/p1/tasks/*', (r) => {
    const key = decodeURIComponent(r.request().url().split('/').pop() ?? '');
    const body = r.request().postDataJSON() as Record<string, unknown>;
    state.patches.push({ key, body });
    const i = state.tasks.findIndex((t) => t.key === key);
    const { note, ...fields } = body;
    const next = { ...state.tasks[i], ...fields } as Task;
    if (note) next['notes'] = `${state.tasks[i]['notes']}\n[2026-10-14T00:00:00Z a@b.c] ${note}`;
    if (next.status === 'done') next['closed_at'] = '2026-10-14T00:00:00Z';
    state.tasks[i] = next;
    return r.fulfill({ json: next });
  });
  return state;
}

test('work: project list shows counts and links to the board', async ({ page }) => {
  await install(page);
  await page.goto('/work');
  const card = page.getByTestId('work-project-card');
  await expect(card).toHaveCount(1);
  await expect(card).toContainText('ManyForge');
  await expect(card.getByTestId('count-todo')).toHaveText('2');
  await expect(card.getByTestId('count-operator')).toHaveText('2');
  await card.click();
  await expect(page).toHaveURL(/\/work\/b1\/p1$/);
  await expect(page.getByTestId('work-board')).toBeVisible();
});

test('work: board columns hold tasks by status; lane filter narrows them', async ({ page }) => {
  await install(page);
  await page.goto('/work/b1/p1');
  const col = (s: string) => page.getByTestId(`work-col-${s}`).getByTestId('work-card');
  await expect(col('todo')).toHaveCount(2);
  await expect(col('todo').first()).toContainText('Rotate the Stripe live key'); // P0 first
  await expect(col('in_progress')).toContainText(['Write the mf CLI']);
  await expect(col('blocked')).toContainText(['Blocked by mf-oper01']);
  await expect(col('done')).toContainText(['Bootstrap repo']);
  await expect(page.getByTestId('work-board')).not.toContainText('Dark-mode polish');

  await page.getByTestId('work-lane-operator').click();
  await expect(page.getByTestId('work-card')).toHaveCount(1);
  await expect(page.getByTestId('work-card')).toHaveAttribute('data-lane', 'operator');
  await page.getByTestId('work-lane-agent').click();
  await expect(page.getByTestId('work-card')).toHaveCount(4);
  await page.getByTestId('work-lane-all').click();
  await expect(page.getByTestId('work-card')).toHaveCount(5);
});

test('work: moving a card with its status select PATCHes the new status', async ({ page }) => {
  const state = await install(page);
  await page.goto('/work/b1/p1');
  await page
    .locator('[data-key="mf-todo01"]')
    .getByTestId('work-card-status')
    .selectOption('in_progress');
  await expect
    .poll(() => state.patches)
    .toEqual([{ key: 'mf-todo01', body: { status: 'in_progress' } }]);
  await expect(page.getByTestId('work-col-in_progress')).toContainText('Wire the sync endpoint');
});

test('work: operator tab lists only open operator tasks; Mark done sends status done', async ({
  page,
}) => {
  const state = await install(page);
  await page.goto('/work/b1/p1');
  await expect(page.getByTestId('work-operator-count')).toHaveText('2');
  await page.getByTestId('work-tab-operator').click();
  await expect(page).toHaveURL(/tab=operator/);
  const items = page.getByTestId('work-operator-item');
  await expect(items).toHaveCount(2);
  await expect(items.first()).toContainText('Rotate the Stripe live key');
  await expect(items.first().getByTestId('work-operator-description')).toContainText(
    'Paste it into the prod secret.',
  );
  await expect(page.getByTestId('work-operator-list')).not.toContainText('Wire the sync endpoint');

  await items.first().getByTestId('work-operator-done').click();
  await expect.poll(() => state.patches).toEqual([{ key: 'mf-oper01', body: { status: 'done' } }]);
  await expect(items).toHaveCount(1);
  await expect(page.getByTestId('work-operator-done-row')).toContainText(
    'Rotate the Stripe live key',
  );
  // The nav badge re-reads the summary after the mutation.
  await expect(page.getByTestId('nav-work').locator('.nav-badge')).toHaveText('1');
});

test('work: creating a task from the Operator tab POSTs lane operator', async ({ page }) => {
  const state = await install(page);
  await page.goto('/work/b1/p1?tab=operator');
  await page.getByTestId('work-new-task').click();
  const dialog = page.getByTestId('work-new-task-dialog');
  await expect(dialog).toContainText('New operator task');
  await expect(dialog.getByTestId('work-new-lane')).toHaveValue('operator');
  await dialog.getByTestId('work-new-title').fill('Click install on the GitHub App');
  await dialog
    .getByTestId('work-new-description')
    .fill('github.com/apps/manyforge-review → Install');
  await dialog.getByTestId('work-new-submit').click();
  await expect.poll(() => state.creates.length).toBe(1);
  expect(state.creates[0]).toMatchObject({
    title: 'Click install on the GitHub App',
    description: 'github.com/apps/manyforge-review → Install',
    lane: 'operator',
    status: 'todo',
  });
  await expect(dialog).toHaveCount(0);
  await expect(page.getByTestId('work-operator-item')).toHaveCount(3);
});

test('work: backlog "Move to To do" PATCHes status todo', async ({ page }) => {
  const state = await install(page);
  await page.goto('/work/b1/p1');
  await page.getByTestId('work-tab-backlog').click();
  const row = page.locator('[data-testid="work-backlog-row"][data-key="mf-back01"]');
  await expect(row).toContainText('Dark-mode polish');
  await row.getByTestId('work-backlog-promote').click();
  await expect.poll(() => state.patches).toEqual([{ key: 'mf-back01', body: { status: 'todo' } }]);
  await expect(row).toHaveCount(0);
});

test('work: task detail edits fields and appends a note', async ({ page }) => {
  const state = await install(page);
  await page.goto('/work/b1/p1');
  await page.locator('[data-key="mf-prog01"]').getByTestId('work-task-open').click();
  const dialog = page.getByTestId('work-task-dialog');
  await expect(dialog.getByTestId('work-task-dialog-key')).toHaveText('mf-prog01');
  await dialog.getByTestId('work-task-priority').selectOption({ label: 'P0 · critical' });
  await dialog.getByTestId('work-task-save').click();
  await expect.poll(() => state.patches[0]).toEqual({ key: 'mf-prog01', body: { priority: 0 } });

  await dialog.getByTestId('work-task-note-input').fill('Pairing on this now');
  await dialog.getByTestId('work-task-add-note').click();
  await expect
    .poll(() => state.patches[1])
    .toEqual({ key: 'mf-prog01', body: { note: 'Pairing on this now' } });
  await expect(dialog.getByTestId('work-task-notes')).toContainText('Pairing on this now');
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
});

test('work: creating a token shows the plaintext once', async ({ page }) => {
  await install(page);
  const tokens: Record<string, unknown>[] = [
    {
      id: 't1',
      name: 'laptop',
      prefix: 'mfw_ab12',
      created_at: '2026-10-01T00:00:00Z',
      last_used_at: null,
      revoked_at: null,
    },
  ];
  await page.route('**/api/v1/businesses/b1/work/projects/p1/tokens', (r) => {
    if (r.request().method() === 'POST') {
      const name = (r.request().postDataJSON() as { name: string }).name;
      return r.fulfill({
        status: 201,
        json: {
          id: 't2',
          name,
          prefix: 'mfw_cd34',
          created_at: '2026-10-14T00:00:00Z',
          token: 'mfw_cd34_plaintext_secret',
        },
      });
    }
    return r.fulfill({ json: { items: tokens } });
  });
  await page.goto('/work/b1/p1?tab=connect');
  await expect(page.getByTestId('work-cmd-init')).toContainText('--project manyforge');
  await expect(page.getByTestId('work-cmd-login')).toHaveText(
    /^mf login --server http:\/\/localhost:\d+ --token mfw_…$/,
  );
  await expect(page.getByTestId('work-token-row')).toHaveCount(1);
  await page.getByTestId('work-token-name').fill('ci-runner');
  await page.getByTestId('work-token-create').click();
  await expect(page.getByTestId('work-token-login-cmd')).toHaveText(
    /^mf login --server http:\/\/localhost:\d+ --token mfw_cd34_plaintext_secret$/,
  );
  await expect(page.getByTestId('work-token-row')).toHaveCount(2);
  await page.getByTestId('work-token-dismiss').click();
  await expect(page.getByTestId('work-token-once')).toHaveCount(0);
  await expect(page.locator('body')).not.toContainText('mfw_cd34_plaintext_secret');
});

test('work: nav badge shows the open operator count', async ({ page }) => {
  await install(page, { openOperator: 4 });
  await page.goto('/dashboard');
  await expect(page.getByTestId('nav-work').locator('.nav-badge')).toHaveText('4');
});
