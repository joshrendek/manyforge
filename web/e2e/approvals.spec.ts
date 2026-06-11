import { expect, test } from '@playwright/test';

const profile = { id: '1', email: 'a@b.c', display_name: 'A', email_verified: true, status: 'active' };
const biz = { items: [{ id: 'b1', parent_id: null, tenant_root_id: 'b1', name: 'Acme', status: 'active', is_tenant_root: true }], next_cursor: null };
const item = { id: 'a1', agent_run_id: 'r1', tool: 'transition_external_status', effect_class: 3, state: 'pending', expires_at: '2026-07-01T00:00:00Z', summary: 'Transition ticket 7bbeb32e → closed' };

test('approvals queue: renders, approve removes the row', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
  let listed = 0;
  await page.route('**/api/v1/businesses/b1/approvals', (r) => {
    listed++;
    return r.fulfill({ json: { items: listed === 1 ? [item] : [] } });
  });
  await page.route('**/api/v1/businesses/b1/approvals/a1/approve', (r) => r.fulfill({ json: { ...item, state: 'approved' } }));

  await page.goto('/approvals');
  await expect(page.getByTestId('approval-summary')).toContainText('Transition ticket');
  await expect(page.getByTestId('approval-effect')).toContainText('Irreversible');
  await page.getByTestId('approval-approve').click();
  await expect(page.getByTestId('approval-row')).toHaveCount(0);
});

test('approvals queue: 409 surfaces and re-lists', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('mf_access', 'tok'));
  await page.route('**/api/v1/me', (r) => r.fulfill({ json: profile }));
  await page.route('**/api/v1/businesses', (r) => r.fulfill({ json: biz }));
  await page.route('**/api/v1/businesses/b1/approvals', (r) => r.fulfill({ json: { items: [item] } }));
  await page.route('**/api/v1/businesses/b1/approvals/a1/deny', (r) => r.fulfill({ status: 409, json: { code: 'CONFLICT', message: 'already decided' } }));

  await page.goto('/approvals');
  await page.getByTestId('approval-deny').click();
  await expect(page.getByTestId('toast')).toContainText(/already decided|refresh/i);
});
