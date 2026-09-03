import { describe, expect, it } from 'vitest';
import { AutomationGraph, AutomationNode } from '../../../../core/automations.service';
import { layoutGraph } from './layout';

const node = (id: string, kind: AutomationNode['kind'] = 'wait'): AutomationNode => ({
  id,
  kind,
  config: kind === 'trigger' ? { type: 'list_joined', list_id: 'list' } : kind === 'condition' ? { predicate: { type: 'has_tag', tag: 'vip' } } : kind === 'exit' ? {} : { mode: 'duration', seconds: 60 },
});
const edge = (id: string, from: string, to: string, branch: 'yes' | 'no' | null = null) => ({ id, from, to, branch });

function positions(graph: AutomationGraph): Map<string, ReturnType<typeof layoutGraph>['nodes'][number]> {
  return new Map(layoutGraph(graph).nodes.map((item) => [item.id, item]));
}

describe('layoutGraph', () => {
  it('orders a linear graph from top to bottom', () => {
    const p = positions({ nodes: [node('t', 'trigger'), node('w'), node('x', 'exit')], edges: [edge('a', 't', 'w'), edge('b', 'w', 'x')] });
    expect(p.get('t')!.y).toBeLessThan(p.get('w')!.y);
    expect(p.get('w')!.y).toBeLessThan(p.get('x')!.y);
  });

  it('places Yes left of No in a branch', () => {
    const p = positions({ nodes: [node('t', 'trigger'), node('c', 'condition'), node('y', 'exit'), node('n', 'exit')], edges: [edge('a', 't', 'c'), edge('b', 'c', 'y', 'yes'), edge('c', 'c', 'n', 'no')] });
    expect(p.get('y')!.x).toBeLessThan(p.get('n')!.x);
  });

  it('keeps nested branch ranks from overlapping', () => {
    const graph: AutomationGraph = {
      nodes: [node('t', 'trigger'), node('a', 'condition'), node('b', 'condition'), node('x', 'exit'), node('y', 'exit'), node('z', 'exit')],
      edges: [edge('1', 't', 'a'), edge('2', 'a', 'b', 'yes'), edge('3', 'a', 'z', 'no'), edge('4', 'b', 'x', 'yes'), edge('5', 'b', 'y', 'no')],
    };
    const layout = layoutGraph(graph);
    for (const rank of new Set(layout.nodes.map((item) => item.rank))) {
      const row = layout.nodes.filter((item) => item.rank === rank).sort((a, b) => a.x - b.x);
      for (let i = 1; i < row.length; i++) expect(row[i].x).toBeGreaterThanOrEqual(row[i - 1].x + row[i - 1].w + 40);
    }
  });

  it('puts a merge after both parents', () => {
    const p = positions({ nodes: [node('t', 'trigger'), node('c', 'condition'), node('y'), node('n'), node('m', 'exit')], edges: [edge('1', 't', 'c'), edge('2', 'c', 'y', 'yes'), edge('3', 'c', 'n', 'no'), edge('4', 'y', 'm'), edge('5', 'n', 'm')] });
    expect(p.get('m')!.rank).toBeGreaterThan(p.get('y')!.rank);
    expect(p.get('m')!.rank).toBeGreaterThan(p.get('n')!.rank);
  });

  it('lays out cycles and orphans without throwing', () => {
    const graph: AutomationGraph = { nodes: [node('t', 'trigger'), node('a'), node('b'), node('orphan', 'exit')], edges: [edge('1', 't', 'a'), edge('2', 'a', 'b'), edge('3', 'b', 'a')] };
    const layout = layoutGraph(graph);
    expect(layout.nodes.map((item) => item.id).sort()).toEqual(['a', 'b', 'orphan', 't']);
    expect(layout.width).toBeGreaterThan(0);
  });
});
