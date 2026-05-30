export interface Business {
  id: string;
  parent_id: string | null;
  tenant_root_id: string;
  name: string;
  status: string;
  is_tenant_root: boolean;
}

export interface TreeNode {
  business: Business;
  depth: number;
  children: TreeNode[];
}

export interface Row {
  business: Business;
  depth: number;
  hasChildren: boolean;
  collapsed: boolean;
}

const byName = (a: TreeNode, b: TreeNode) =>
  a.business.name.localeCompare(b.business.name, undefined, { sensitivity: 'base' });

// buildTree turns the flat business list (each row carrying parent_id) into a
// sorted forest. A row whose parent is not present in the set — e.g. an ancestor
// hidden by RLS — is surfaced as a root so it is never silently dropped.
export function buildTree(items: Business[]): TreeNode[] {
  const present = new Set(items.map((b) => b.id));
  const childrenOf = new Map<string, Business[]>();
  const roots: Business[] = [];

  for (const b of items) {
    const isRoot = b.parent_id === null || !present.has(b.parent_id);
    if (isRoot) {
      roots.push(b);
    } else {
      const siblings = childrenOf.get(b.parent_id!) ?? [];
      siblings.push(b);
      childrenOf.set(b.parent_id!, siblings);
    }
  }

  const build = (b: Business, depth: number): TreeNode => ({
    business: b,
    depth,
    children: (childrenOf.get(b.id) ?? []).map((c) => build(c, depth + 1)).sort(byName),
  });

  return roots.map((r) => build(r, 0)).sort(byName);
}

// flatten produces pre-order rows for rendering. Descendants of a collapsed node
// are omitted, but the collapsed node itself is still emitted (with hasChildren).
export function flatten(tree: TreeNode[], collapsed: ReadonlySet<string>): Row[] {
  const rows: Row[] = [];
  const walk = (nodes: TreeNode[]) => {
    for (const node of nodes) {
      const isCollapsed = collapsed.has(node.business.id);
      rows.push({
        business: node.business,
        depth: node.depth,
        hasChildren: node.children.length > 0,
        collapsed: isCollapsed,
      });
      if (!isCollapsed) {
        walk(node.children);
      }
    }
  };
  walk(tree);
  return rows;
}
