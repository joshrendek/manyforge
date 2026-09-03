import { describe, expect, it } from 'vitest';
import { AutomationGraph } from '../../../../core/automations.service';
import {
  deleteNode,
  insertNode,
  retargetExit,
  starterGraph,
  validateGraph,
} from './graph-ops';

function ids(...values: string[]): () => string {
  let index = 0;
  return () => values[index++];
}

const listId = '11111111-1111-4111-8111-111111111111';

const base: AutomationGraph = {
  nodes: [
    { id: 'trigger', kind: 'trigger', config: { type: 'list_joined', list_id: listId } },
    { id: 'exit', kind: 'exit', config: {} },
  ],
  edges: [{ id: 'edge', from: 'trigger', to: 'exit', branch: null }],
};

describe('graph operations', () => {
  it('creates a usable starter graph', () => {
    const graph = starterGraph(listId, ids('trigger', 'exit', 'edge'));
    expect(graph.nodes.map(({ id, kind, config }) => ({ id, kind, config }))).toEqual(base.nodes);
    expect(graph.edges).toEqual(base.edges);
    expect(validateGraph(graph)).toEqual([]);
  });

  it('inserts a plain node by splitting an edge', () => {
    const result = insertNode(base, 'edge', 'wait', {}, ids('wait', 'before', 'after'));
    expect(result.selectedId).toBe('wait');
    expect(result.graph.edges).toEqual([
      { id: 'before', from: 'trigger', to: 'wait', branch: null },
      { id: 'after', from: 'wait', to: 'exit', branch: null },
    ]);
  });

  it('inserts a condition with Yes continuation and a fresh No exit', () => {
    const result = insertNode(
      base,
      'edge',
      'condition',
      {},
      ids('condition', 'before', 'yes', 'no-exit', 'no-edge'),
    );
    expect(result.graph.edges).toEqual([
      { id: 'before', from: 'trigger', to: 'condition', branch: null },
      { id: 'yes', from: 'condition', to: 'exit', branch: 'yes' },
      { id: 'no-edge', from: 'condition', to: 'no-exit', branch: 'no' },
    ]);
  });

  it('deletes a condition and its no-only subtree while preserving the yes path', () => {
    const graph: AutomationGraph = {
      nodes: [
        ...base.nodes,
        { id: 'condition', kind: 'condition', config: { predicate: { type: 'has_tag', tag: 'vip' } } },
        { id: 'no-wait', kind: 'wait', config: { mode: 'duration', seconds: 60 } },
        { id: 'no-exit', kind: 'exit', config: {} },
      ],
      edges: [
        { id: 'in', from: 'trigger', to: 'condition', branch: null },
        { id: 'yes', from: 'condition', to: 'exit', branch: 'yes' },
        { id: 'no', from: 'condition', to: 'no-wait', branch: 'no' },
        { id: 'no-end', from: 'no-wait', to: 'no-exit', branch: null },
      ],
    };
    const result = deleteNode(graph, 'condition');
    expect(result.removedIds).toEqual(['condition', 'no-wait', 'no-exit']);
    expect(result.graph.edges).toEqual([{ id: 'in', from: 'trigger', to: 'exit', branch: null }]);
  });

  it('retargets an exit to create a merge and removes the old exit', () => {
    const graph: AutomationGraph = {
      ...base,
      nodes: [...base.nodes, { id: 'wait', kind: 'wait', config: { mode: 'duration', seconds: 60 } }],
    };
    const result = retargetExit(graph, 'exit', 'wait');
    expect(result.graph.nodes.some((node) => node.id === 'exit')).toBe(false);
    expect(result.graph.edges[0].to).toBe('wait');
  });

  it('reports structural and configuration problems', () => {
    const graph: AutomationGraph = {
      nodes: [
        { id: 'trigger', kind: 'trigger', config: { type: 'list_joined', list_id: '' } },
        { id: 'orphan', kind: 'exit', config: {} },
      ],
      edges: [],
    };
    expect(validateGraph(graph).map((issue) => issue.code)).toEqual([
      'invalid_config',
      'unreachable_node',
    ]);
  });
});
