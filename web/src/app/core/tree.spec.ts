import { describe, expect, it } from 'vitest';
import { Business, buildTree, flatten } from './tree';

// A small factory keeps the fixtures readable.
function biz(id: string, name: string, parent: string | null, root = id, status = 'active'): Business {
  return {
    id,
    name,
    parent_id: parent,
    tenant_root_id: root,
    status,
    is_tenant_root: parent === null,
  };
}

describe('buildTree', () => {
  it('returns an empty array for no businesses', () => {
    expect(buildTree([])).toEqual([]);
  });

  it('returns a single root at depth 0 with no children', () => {
    const tree = buildTree([biz('a', 'Acme', null)]);
    expect(tree).toHaveLength(1);
    expect(tree[0].business.id).toBe('a');
    expect(tree[0].depth).toBe(0);
    expect(tree[0].children).toEqual([]);
  });

  it('nests master -> child -> grandchild with increasing depth', () => {
    const tree = buildTree([
      biz('root', 'Root', null),
      biz('child', 'Child', 'root'),
      biz('grand', 'Grand', 'child'),
    ]);
    expect(tree).toHaveLength(1);
    const root = tree[0];
    expect(root.depth).toBe(0);
    expect(root.children).toHaveLength(1);
    const child = root.children[0];
    expect(child.business.id).toBe('child');
    expect(child.depth).toBe(1);
    expect(child.children[0].business.id).toBe('grand');
    expect(child.children[0].depth).toBe(2);
  });

  it('sorts sibling roots and children alphabetically (case-insensitive)', () => {
    const tree = buildTree([
      biz('b', 'beta', null),
      biz('a', 'Alpha', null),
      biz('a2', 'zeta', 'a'),
      biz('a1', 'Gamma', 'a'),
    ]);
    expect(tree.map((n) => n.business.name)).toEqual(['Alpha', 'beta']);
    expect(tree[0].children.map((n) => n.business.name)).toEqual(['Gamma', 'zeta']);
  });

  it('surfaces an orphan (parent not in the visible set) as a root rather than dropping it', () => {
    // RLS can hide an ancestor; we must never silently lose a row the caller can see.
    const tree = buildTree([biz('orphan', 'Orphan', 'missing-parent', 'some-root')]);
    expect(tree).toHaveLength(1);
    expect(tree[0].business.id).toBe('orphan');
    expect(tree[0].depth).toBe(0);
  });
});

describe('flatten', () => {
  const tree = buildTree([
    biz('root', 'Root', null),
    biz('child', 'Child', 'root'),
    biz('grand', 'Grand', 'child'),
  ]);

  it('pre-order flattens to rows with depth and hasChildren', () => {
    const rows = flatten(tree, new Set());
    expect(rows.map((r) => r.business.id)).toEqual(['root', 'child', 'grand']);
    expect(rows.map((r) => r.depth)).toEqual([0, 1, 2]);
    expect(rows.map((r) => r.hasChildren)).toEqual([true, true, false]);
  });

  it('hides descendants of a collapsed node but still emits the collapsed node', () => {
    const rows = flatten(tree, new Set(['child']));
    expect(rows.map((r) => r.business.id)).toEqual(['root', 'child']);
    const childRow = rows.find((r) => r.business.id === 'child')!;
    expect(childRow.hasChildren).toBe(true);
    expect(childRow.collapsed).toBe(true);
  });
});
