import { AutomationEdge, AutomationGraph, AutomationNodeKind } from '../../../../core/automations.service';

export interface NodeSize { w: number; h: number }
export interface LayoutNode extends NodeSize { id: string; x: number; y: number; rank: number }
export interface LayoutEdge {
  id: string;
  path: string;
  plus: { x: number; y: number };
  label: { x: number; y: number; text: 'Yes' | 'No' } | null;
}
export interface GraphLayout {
  nodes: LayoutNode[];
  edges: LayoutEdge[];
  width: number;
  height: number;
}

export const NODE_SIZE: Record<AutomationNodeKind, NodeSize> = {
  trigger: { w: 220, h: 64 },
  condition: { w: 220, h: 72 },
  send_email: { w: 220, h: 80 },
  wait: { w: 200, h: 56 },
  add_tag: { w: 200, h: 56 },
  remove_tag: { w: 200, h: 56 },
  exit: { w: 200, h: 56 },
};

const GAP_X = 40;
const GAP_Y = 64;
const PADDING = 32;

export function layoutGraph(
  graph: AutomationGraph,
  sizes: Record<AutomationNodeKind, NodeSize> = NODE_SIZE,
): GraphLayout {
  if (!graph.nodes.length) return { nodes: [], edges: [], width: 0, height: 0 };

  const nodeById = new Map(graph.nodes.map((node) => [node.id, node]));
  const usableEdges = graph.edges.filter((edge) => nodeById.has(edge.from) && nodeById.has(edge.to));
  const backEdges = findBackEdges(graph.nodes.map((node) => node.id), usableEdges);
  const dagEdges = usableEdges.filter((edge) => !backEdges.has(edge.id));
  const outgoing = adjacency(dagEdges, 'from');
  const incoming = adjacency(dagEdges, 'to');
  const rank = longestPathRanks(graph.nodes.map((node) => node.id), dagEdges, outgoing, incoming);
  const maxReachableRank = Math.max(0, ...rank.values());
  const trigger = graph.nodes.find((node) => node.kind === 'trigger');
  const reachable = trigger ? reachableIds(trigger.id, outgoing) : new Set<string>();
  for (const node of graph.nodes) {
    if (!reachable.has(node.id)) rank.set(node.id, maxReachableRank + 1);
  }

  const treeParent = new Map<string, string>();
  const treeParentEdge = new Map<string, AutomationEdge>();
  for (const node of graph.nodes) {
    const candidates = [...(incoming.get(node.id) ?? [])].sort((a, b) => {
      const rankDiff = (rank.get(b.from) ?? 0) - (rank.get(a.from) ?? 0);
      return rankDiff || a.id.localeCompare(b.id);
    });
    if (candidates[0]) {
      treeParent.set(node.id, candidates[0].from);
      treeParentEdge.set(node.id, candidates[0]);
    }
  }
  const children = new Map<string, string[]>();
  for (const [child, parent] of treeParent) {
    children.set(parent, [...(children.get(parent) ?? []), child]);
  }
  for (const [parent, ids] of children) {
    ids.sort((a, b) => {
      const branchDiff = branchOrder(treeParentEdge.get(a)?.branch ?? null) - branchOrder(treeParentEdge.get(b)?.branch ?? null);
      return branchDiff || (treeParentEdge.get(a)?.id ?? '').localeCompare(treeParentEdge.get(b)?.id ?? '');
    });
    children.set(parent, ids);
  }

  const subtreeWidth = new Map<string, number>();
  const measure = (id: string): number => {
    if (subtreeWidth.has(id)) return subtreeWidth.get(id)!;
    const node = nodeById.get(id)!;
    const childWidths = (children.get(id) ?? []).map(measure);
    const total = childWidths.reduce((sum, width) => sum + width, 0) + Math.max(0, childWidths.length - 1) * GAP_X;
    const width = Math.max(sizes[node.kind].w, total);
    subtreeWidth.set(id, width);
    return width;
  };

  const x = new Map<string, number>();
  const place = (id: string, left: number): void => {
    const node = nodeById.get(id)!;
    const childIds = children.get(id) ?? [];
    if (!childIds.length) {
      x.set(id, left + (measure(id) - sizes[node.kind].w) / 2);
      return;
    }
    let cursor = left;
    for (const childId of childIds) {
      place(childId, cursor);
      cursor += measure(childId) + GAP_X;
    }
    const first = childIds[0];
    const last = childIds[childIds.length - 1];
    const lastNode = nodeById.get(last)!;
    const centre = ((x.get(first) ?? left) + (x.get(last) ?? left) + sizes[lastNode.kind].w) / 2;
    x.set(id, centre - sizes[node.kind].w / 2);
  };

  const roots = graph.nodes
    .filter((node) => !treeParent.has(node.id))
    .sort((a, b) => (a.kind === 'trigger' ? -1 : b.kind === 'trigger' ? 1 : a.id.localeCompare(b.id)));
  let rootCursor = 0;
  for (const root of roots) {
    place(root.id, rootCursor);
    rootCursor += measure(root.id) + GAP_X * 2;
  }
  for (const node of graph.nodes) if (!x.has(node.id)) x.set(node.id, rootCursor += GAP_X);

  const ranks = new Map<number, string[]>();
  for (const node of graph.nodes) {
    const value = rank.get(node.id) ?? maxReachableRank + 1;
    ranks.set(value, [...(ranks.get(value) ?? []), node.id]);
  }
  for (const ids of ranks.values()) {
    ids.sort((a, b) => (x.get(a) ?? 0) - (x.get(b) ?? 0) || a.localeCompare(b));
    let right = Number.NEGATIVE_INFINITY;
    for (const id of ids) {
      const node = nodeById.get(id)!;
      const nextX = Math.max(x.get(id) ?? 0, right + GAP_X);
      x.set(id, nextX);
      right = nextX + sizes[node.kind].w;
    }
  }

  const rowHeight = new Map<number, number>();
  for (const node of graph.nodes) {
    const value = rank.get(node.id) ?? 0;
    rowHeight.set(value, Math.max(rowHeight.get(value) ?? 0, sizes[node.kind].h));
  }
  const yByRank = new Map<number, number>();
  let cursorY = 0;
  for (const value of [...ranks.keys()].sort((a, b) => a - b)) {
    yByRank.set(value, cursorY);
    cursorY += (rowHeight.get(value) ?? 0) + GAP_Y;
  }

  const rawNodes = graph.nodes.map((node) => ({
    id: node.id,
    x: x.get(node.id) ?? 0,
    y: yByRank.get(rank.get(node.id) ?? 0) ?? 0,
    rank: rank.get(node.id) ?? 0,
    ...sizes[node.kind],
  }));
  const minX = Math.min(...rawNodes.map((node) => node.x));
  const minY = Math.min(...rawNodes.map((node) => node.y));
  const nodes = rawNodes
    .map((node) => ({ ...node, x: node.x - minX + PADDING, y: node.y - minY + PADDING }))
    .sort((a, b) => a.rank - b.rank || a.x - b.x || a.id.localeCompare(b.id));
  const placed = new Map(nodes.map((node) => [node.id, node]));
  const edges = usableEdges.flatMap((edge): LayoutEdge[] => {
    const from = placed.get(edge.from);
    const to = placed.get(edge.to);
    if (!from || !to) return [];
    const start = { x: from.x + from.w / 2, y: from.y + from.h };
    const end = { x: to.x + to.w / 2, y: to.y };
    const bend = Math.max(24, (end.y - start.y) / 2);
    const c1 = { x: start.x, y: start.y + bend };
    const c2 = { x: end.x, y: end.y - bend };
    return [{
      id: edge.id,
      path: `M ${start.x} ${start.y} C ${c1.x} ${c1.y}, ${c2.x} ${c2.y}, ${end.x} ${end.y}`,
      plus: cubicPoint(start, c1, c2, end, 0.5),
      label: edge.branch ? { ...cubicPoint(start, c1, c2, end, 0.25), text: edge.branch === 'yes' ? 'Yes' : 'No' } : null,
    }];
  });
  return {
    nodes,
    edges,
    width: Math.max(...nodes.map((node) => node.x + node.w)) + PADDING,
    height: Math.max(...nodes.map((node) => node.y + node.h)) + PADDING,
  };
}

function adjacency(edges: AutomationEdge[], key: 'from' | 'to'): Map<string, AutomationEdge[]> {
  const map = new Map<string, AutomationEdge[]>();
  for (const edge of edges) map.set(edge[key], [...(map.get(edge[key]) ?? []), edge]);
  for (const [id, values] of map) {
    values.sort((a, b) => branchOrder(a.branch) - branchOrder(b.branch) || a.id.localeCompare(b.id));
    map.set(id, values);
  }
  return map;
}

function branchOrder(branch: AutomationEdge['branch']): number {
  return branch === 'yes' ? 0 : branch === 'no' ? 1 : 2;
}

function findBackEdges(ids: string[], edges: AutomationEdge[]): Set<string> {
  const colors = new Map<string, number>();
  const ignored = new Set<string>();
  const outgoing = adjacency(edges, 'from');
  const visit = (id: string): void => {
    colors.set(id, 1);
    for (const edge of outgoing.get(id) ?? []) {
      if (colors.get(edge.to) === 1) ignored.add(edge.id);
      else if (colors.get(edge.to) !== 2) visit(edge.to);
    }
    colors.set(id, 2);
  };
  for (const id of [...ids].sort()) if (!colors.has(id)) visit(id);
  return ignored;
}

function longestPathRanks(
  ids: string[],
  edges: AutomationEdge[],
  outgoing: Map<string, AutomationEdge[]>,
  incoming: Map<string, AutomationEdge[]>,
): Map<string, number> {
  const degree = new Map(ids.map((id) => [id, (incoming.get(id) ?? []).length]));
  const rank = new Map(ids.map((id) => [id, 0]));
  const queue = ids.filter((id) => degree.get(id) === 0).sort();
  while (queue.length) {
    const id = queue.shift()!;
    for (const edge of outgoing.get(id) ?? []) {
      rank.set(edge.to, Math.max(rank.get(edge.to) ?? 0, (rank.get(id) ?? 0) + 1));
      degree.set(edge.to, (degree.get(edge.to) ?? 1) - 1);
      if (degree.get(edge.to) === 0) {
        queue.push(edge.to);
        queue.sort();
      }
    }
  }
  // An edge can only remain after malformed input; keep its nodes deterministic.
  for (const edge of edges) rank.set(edge.to, Math.max(rank.get(edge.to) ?? 0, Math.min(ids.length, (rank.get(edge.from) ?? 0) + 1)));
  return rank;
}

function reachableIds(root: string, outgoing: Map<string, AutomationEdge[]>): Set<string> {
  const seen = new Set([root]);
  const queue = [root];
  while (queue.length) {
    const id = queue.shift()!;
    for (const edge of outgoing.get(id) ?? []) {
      if (!seen.has(edge.to)) {
        seen.add(edge.to);
        queue.push(edge.to);
      }
    }
  }
  return seen;
}

function cubicPoint(
  p0: { x: number; y: number },
  p1: { x: number; y: number },
  p2: { x: number; y: number },
  p3: { x: number; y: number },
  t: number,
): { x: number; y: number } {
  const u = 1 - t;
  return {
    x: u ** 3 * p0.x + 3 * u ** 2 * t * p1.x + 3 * u * t ** 2 * p2.x + t ** 3 * p3.x,
    y: u ** 3 * p0.y + 3 * u ** 2 * t * p1.y + 3 * u * t ** 2 * p2.y + t ** 3 * p3.y,
  };
}
