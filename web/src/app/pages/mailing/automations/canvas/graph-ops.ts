import {
  AutomationEdge,
  AutomationGraph,
  AutomationIssue,
  AutomationNode,
  AutomationNodeConfig,
  AutomationNodeKind,
  ConditionConfig,
  SendEmailConfig,
  TriggerConfig,
  WaitConfig,
} from '../../../../core/automations.service';

export interface GraphReferences {
  listId?: string;
  templateId?: string;
  sendNodeId?: string;
}

export interface GraphMutation {
  graph: AutomationGraph;
  selectedId: string | null;
  removedIds: string[];
}

export type IdFactory = () => string;

const defaultId: IdFactory = () => crypto.randomUUID();
const idPattern = /^[a-z0-9_-]{1,64}$/;
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const nodeKinds: AutomationNodeKind[] = ['trigger', 'send_email', 'wait', 'condition', 'add_tag', 'remove_tag', 'exit'];

export function defaultConfig(
  kind: AutomationNodeKind,
  refs: GraphReferences = {},
): AutomationNodeConfig {
  switch (kind) {
    case 'trigger':
      return { type: 'list_joined', list_id: refs.listId ?? '' };
    case 'send_email':
      return {
        template_id: refs.templateId ?? '',
        track_opens: true,
        track_clicks: true,
      };
    case 'wait':
      return { mode: 'duration', seconds: 3600 };
    case 'condition':
      return {
        predicate: refs.sendNodeId
          ? { type: 'opened_email', node_id: refs.sendNodeId }
          : { type: 'has_tag', tag: '' },
      };
    case 'add_tag':
    case 'remove_tag':
      return { tag: '' };
    case 'exit':
      return {};
  }
}

export function starterGraph(listId: string, ids: IdFactory = defaultId): AutomationGraph {
  const nextId = idAllocator({ nodes: [], edges: [] }, ids);
  const triggerId = nextId();
  const exitId = nextId();
  return {
    nodes: [
      { id: triggerId, kind: 'trigger', name: 'Trigger', config: defaultConfig('trigger', { listId }) },
      { id: exitId, kind: 'exit', name: 'Exit', config: {} },
    ],
    edges: [{ id: nextId(), from: triggerId, to: exitId, branch: null }],
  };
}

export function insertNode(
  graph: AutomationGraph,
  edgeId: string,
  kind: Exclude<AutomationNodeKind, 'trigger'>,
  refs: GraphReferences = {},
  ids: IdFactory = defaultId,
): GraphMutation {
  const edge = graph.edges.find((candidate) => candidate.id === edgeId);
  if (!edge) throw new Error(`Unknown edge: ${edgeId}`);

  const nextId = idAllocator(graph, ids);
  const nodeId = nextId();
  const node: AutomationNode = {
    id: nodeId,
    kind,
    name: defaultName(kind),
    config: defaultConfig(kind, refs),
  };
  const before: AutomationEdge = {
    id: nextId(),
    from: edge.from,
    to: nodeId,
    branch: edge.branch,
  };
  const after: AutomationEdge = {
    id: nextId(),
    from: nodeId,
    to: edge.to,
    branch: kind === 'condition' ? 'yes' : null,
  };
  const nodes = [...graph.nodes, node];
  const edges = graph.edges.filter((candidate) => candidate.id !== edgeId);
  edges.push(before, after);

  if (kind === 'condition') {
    const exitId = nextId();
    nodes.push({ id: exitId, kind: 'exit', name: 'Exit', config: {} });
    edges.push({ id: nextId(), from: nodeId, to: exitId, branch: 'no' });
  }

  return { graph: { nodes, edges }, selectedId: nodeId, removedIds: [] };
}

export function deleteNode(graph: AutomationGraph, nodeId: string): GraphMutation {
  const node = graph.nodes.find((candidate) => candidate.id === nodeId);
  if (!node) throw new Error(`Unknown node: ${nodeId}`);
  if (node.kind === 'trigger') throw new Error('The trigger cannot be deleted.');
  if (node.kind === 'exit' && graph.nodes.filter((candidate) => candidate.kind === 'exit').length <= 1) {
    throw new Error('The only exit cannot be deleted.');
  }

  const incoming = graph.edges.filter((edge) => edge.to === nodeId);
  const outgoing = graph.edges.filter((edge) => edge.from === nodeId);
  let edges = graph.edges.filter((edge) => edge.to !== nodeId && edge.from !== nodeId);

  if (node.kind === 'condition') {
    const originalTrigger = graph.nodes.find((candidate) => candidate.kind === 'trigger');
    const originallyReachable = originalTrigger ? reachableFrom(graph, originalTrigger.id) : new Set<string>();
    const yes = outgoing.find((edge) => edge.branch === 'yes');
    if (yes) {
      edges = [
        ...edges,
        ...incoming.map((edge) => ({ ...edge, to: yes.to })),
      ];
    }
    const provisional: AutomationGraph = {
      nodes: graph.nodes.filter((candidate) => candidate.id !== nodeId),
      edges,
    };
    const trigger = provisional.nodes.find((candidate) => candidate.kind === 'trigger');
    const reachable = trigger ? reachableFrom(provisional, trigger.id) : new Set<string>();
    const removedIds = graph.nodes
      .filter((candidate) => candidate.id === nodeId || (originallyReachable.has(candidate.id) && !reachable.has(candidate.id)))
      .map((candidate) => candidate.id);
    const removed = new Set(removedIds);
    return {
      graph: {
        nodes: graph.nodes.filter((candidate) => !removed.has(candidate.id)),
        edges: edges.filter((edge) => !removed.has(edge.from) && !removed.has(edge.to)),
      },
      selectedId: yes?.to ?? incoming[0]?.from ?? null,
      removedIds,
    };
  }

  const successor = outgoing[0]?.to;
  if (successor) {
    edges = [...edges, ...incoming.map((edge) => ({ ...edge, to: successor }))];
  }
  return {
    graph: {
      nodes: graph.nodes.filter((candidate) => candidate.id !== nodeId),
      edges,
    },
    selectedId: successor ?? incoming[0]?.from ?? null,
    removedIds: [nodeId],
  };
}

export function retargetExit(
  graph: AutomationGraph,
  exitId: string,
  targetId: string,
): GraphMutation {
  const exit = graph.nodes.find((node) => node.id === exitId);
  const target = graph.nodes.find((node) => node.id === targetId);
  if (exit?.kind !== 'exit' || !target || exitId === targetId) {
    throw new Error('Choose a valid continuation target.');
  }
  if (reachableFrom(graph, targetId).has(exitId)) {
    throw new Error('A continuation cannot point back to an ancestor.');
  }
  return {
    graph: {
      nodes: graph.nodes.filter((node) => node.id !== exitId),
      edges: graph.edges.map((edge) => (edge.to === exitId ? { ...edge, to: targetId } : edge)),
    },
    selectedId: targetId,
    removedIds: [exitId],
  };
}

export function continuationTargets(graph: AutomationGraph, exitId: string): AutomationNode[] {
  return graph.nodes.filter(
    (node) => node.id !== exitId && node.kind !== 'trigger' && !reachableFrom(graph, node.id).has(exitId),
  );
}

export function validateGraph(graph: AutomationGraph): AutomationIssue[] {
  const issues: AutomationIssue[] = [];
  const nodes = new Map<string, AutomationNode>();
  const triggers: string[] = [];

  if (graph.nodes.length > 200) {
    issues.push({ code: 'too_many_nodes', message: `Graph has ${graph.nodes.length} nodes; maximum is 200.` });
  }
  for (const node of graph.nodes) {
    if (!idPattern.test(node.id)) nodeIssue(issues, 'invalid_node_id', node.id, 'Node ID is invalid.');
    if (nodes.has(node.id)) nodeIssue(issues, 'duplicate_node_id', node.id, 'Node IDs must be unique.');
    else nodes.set(node.id, node);
    if ((node.name?.length ?? 0) > 200) nodeIssue(issues, 'invalid_node_name', node.id, 'Name is too long.');
    if (!nodeKinds.includes(node.kind)) nodeIssue(issues, 'unknown_node_kind', node.id, 'Unknown node kind.');
    if (node.kind === 'trigger') triggers.push(node.id);
    validateConfig(node, issues);
  }
  if (triggers.length !== 1) issues.push({ code: 'trigger_count', message: 'Graph must contain exactly one trigger.' });

  const edgeIds = new Set<string>();
  const incoming = new Map<string, AutomationEdge[]>();
  const outgoing = new Map<string, AutomationEdge[]>();
  for (const edge of graph.edges) {
    if (!idPattern.test(edge.id)) edgeIssue(issues, 'invalid_edge_id', edge.id, 'Edge ID is invalid.');
    if (edgeIds.has(edge.id)) edgeIssue(issues, 'duplicate_edge_id', edge.id, 'Edge IDs must be unique.');
    edgeIds.add(edge.id);
    if (!nodes.has(edge.from) || !nodes.has(edge.to)) {
      edgeIssue(issues, 'edge_unknown_node', edge.id, 'Edge endpoints must exist.');
      continue;
    }
    if (edge.from === edge.to) edgeIssue(issues, 'self_loop', edge.id, 'Self-loops are not allowed.');
    append(outgoing, edge.from, edge);
    append(incoming, edge.to, edge);
  }

  for (const node of graph.nodes) {
    const edges = outgoing.get(node.id) ?? [];
    if (node.kind === 'condition') {
      const yes = edges.filter((edge) => edge.branch === 'yes').length;
      const no = edges.filter((edge) => edge.branch === 'no').length;
      for (const edge of edges) {
        if (edge.branch === null) edgeIssue(issues, 'condition_branch_required', edge.id, 'Condition edges require a branch.');
        else if (edge.branch !== 'yes' && edge.branch !== 'no') edgeIssue(issues, 'invalid_edge_branch', edge.id, 'Branch must be yes or no.');
      }
      if (yes !== 1 || no !== 1 || edges.length !== 2) {
        nodeIssue(issues, 'condition_branches', node.id, 'Condition needs exactly one Yes and one No branch.');
      }
    } else {
      if (edges.length > 1) nodeIssue(issues, 'invalid_out_degree', node.id, 'This node can have at most one outgoing edge.');
      for (const edge of edges) {
        if (edge.branch !== null) edgeIssue(issues, 'unexpected_edge_branch', edge.id, 'Only conditions can branch.');
      }
    }
  }
  for (const id of triggers) {
    if ((incoming.get(id) ?? []).length) nodeIssue(issues, 'trigger_indegree', id, 'Trigger cannot have incoming edges.');
  }

  if (hasCycle(graph)) issues.push({ code: 'cycle', message: 'Graph must be acyclic.' });
  if (triggers.length === 1) {
    const reachable = reachableFrom(graph, triggers[0]);
    for (const node of graph.nodes) {
      if (!reachable.has(node.id)) nodeIssue(issues, 'unreachable_node', node.id, 'Node must be reachable from the trigger.');
    }
  }

  for (const node of graph.nodes) {
    if (node.kind !== 'condition') continue;
    const predicate = (node.config as ConditionConfig).predicate;
    if (predicate?.type !== 'opened_email' && predicate?.type !== 'clicked_link') continue;
    const referenced = nodes.get(predicate.node_id);
    if (referenced?.kind !== 'send_email' || !reachableFrom(graph, predicate.node_id).has(node.id)) {
      nodeIssue(issues, 'invalid_engagement_ancestor', node.id, 'Choose an earlier email step.');
    }
  }
  return issues;
}

export function summarizeNode(node: AutomationNode): string {
  switch (node.kind) {
    case 'trigger': {
      const config = node.config as TriggerConfig;
      if (config.type === 'list_joined') return 'When someone joins a list';
      if (config.type === 'tag_added') return `When tag “${config.tag || '…'}” is added`;
      return `When event “${config.name || '…'}” arrives`;
    }
    case 'send_email':
      return (node.config as SendEmailConfig).template_id ? 'Send selected template' : 'Choose a template';
    case 'wait': {
      const config = node.config as WaitConfig;
      return config.mode === 'duration' ? `Wait ${formatDuration(config.seconds)}` : `Wait until ${config.time || 'a time'}`;
    }
    case 'condition': {
      const predicate = (node.config as ConditionConfig).predicate;
      const labels: Record<string, string> = {
        opened_email: 'If an email was opened',
        clicked_link: 'If a link was clicked',
        has_tag: 'If subscriber has a tag',
        on_list: 'If subscriber is on a list',
        event_received: 'If an event was received',
      };
      return labels[predicate?.type] ?? 'Choose a condition';
    }
    case 'add_tag':
      return `Add tag “${(node.config as { tag: string }).tag || '…'}”`;
    case 'remove_tag':
      return `Remove tag “${(node.config as { tag: string }).tag || '…'}”`;
    case 'exit':
      return 'End this path';
  }
}

function validateConfig(node: AutomationNode, issues: AutomationIssue[]): void {
  const bad = (message: string) => nodeIssue(issues, 'invalid_config', node.id, message);
  const config = node.config as Record<string, unknown>;
  if (!config || typeof config !== 'object' || Array.isArray(config)) return bad('Configuration must be an object.');
  switch (node.kind) {
    case 'trigger': {
      const type = config['type'];
      if (!['list_joined', 'tag_added', 'event'].includes(String(type)) || !validUuid(config['list_id'])) return bad('Choose a trigger and list.');
      if (type === 'list_joined' && !keysAre(config, ['type', 'list_id'])) return bad('List trigger configuration has unexpected fields.');
      if (type === 'tag_added' && (!keysAre(config, ['type', 'list_id', 'tag']) || !label(config['tag'], 64))) return bad('Enter a tag.');
      if (type === 'event' && (!keysAre(config, ['type', 'list_id', 'name']) || !label(config['name'], 128))) return bad('Enter an event name.');
      break;
    }
    case 'send_email':
      if (!keysAre(config, ['template_id', 'track_opens', 'track_clicks']) || !validUuid(config['template_id']) || typeof config['track_opens'] !== 'boolean' || typeof config['track_clicks'] !== 'boolean') bad('Choose a template and tracking options.');
      break;
    case 'wait':
      if (config['mode'] === 'duration') {
        if (!keysAre(config, ['mode', 'seconds']) || !Number.isInteger(config['seconds']) || Number(config['seconds']) < 60 || Number(config['seconds']) > 31_536_000) bad('Wait must be between one minute and one year.');
      } else if (config['mode'] === 'until') {
        const weekday = config['weekday'];
        if (!keysAre(config, ['mode', 'weekday', 'time', 'timezone']) || !/^([01]\d|2[0-3]):[0-5]\d$/.test(String(config['time'] ?? '')) || !validTimeZone(config['timezone']) || (weekday !== undefined && weekday !== null && (!Number.isInteger(weekday) || Number(weekday) < 1 || Number(weekday) > 7))) bad('Choose a valid day, time, and timezone.');
      } else bad('Choose a wait mode.');
      break;
    case 'condition':
      if (!keysAre(config, ['predicate'])) return bad('Condition configuration has unexpected fields.');
      validatePredicate(node, config['predicate'], issues);
      break;
    case 'add_tag':
    case 'remove_tag':
      if (!keysAre(config, ['tag']) || !label(config['tag'], 64)) bad('Enter a tag.');
      break;
    case 'exit':
      if (Object.keys(config).length) bad('Exit configuration must be empty.');
      break;
  }
}

function validatePredicate(node: AutomationNode, raw: unknown, issues: AutomationIssue[]): void {
  const bad = (message: string) => nodeIssue(issues, 'invalid_config', node.id, message);
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return bad('Choose a condition.');
  const predicate = raw as Record<string, unknown>;
  switch (predicate['type']) {
    case 'opened_email':
      if (!keysAre(predicate, ['type', 'node_id']) || !idPattern.test(String(predicate['node_id'] ?? ''))) bad('Choose an earlier email step.');
      break;
    case 'clicked_link':
      if (!keysAre(predicate, ['type', 'node_id', 'url']) || !idPattern.test(String(predicate['node_id'] ?? ''))) bad('Choose an earlier email step.');
      if (predicate['url'] !== null && predicate['url'] !== undefined && !validHttpUrl(String(predicate['url']))) bad('Enter a valid HTTP URL.');
      break;
    case 'has_tag':
      if (!keysAre(predicate, ['type', 'tag']) || !label(predicate['tag'], 64)) bad('Enter a tag.');
      break;
    case 'on_list':
      if (!keysAre(predicate, ['type', 'list_id']) || !validUuid(predicate['list_id'])) bad('Choose a list.');
      break;
    case 'event_received':
      if (!keysAre(predicate, ['type', 'name', 'within_seconds']) || !label(predicate['name'], 128)) bad('Enter an event name.');
      if (predicate['within_seconds'] !== null && predicate['within_seconds'] !== undefined && (!Number.isInteger(predicate['within_seconds']) || Number(predicate['within_seconds']) < 1 || Number(predicate['within_seconds']) > 31_536_000)) bad('Event window must be between one second and one year.');
      break;
    default:
      bad('Choose a condition.');
  }
}

function reachableFrom(graph: AutomationGraph, from: string): Set<string> {
  const seen = new Set<string>([from]);
  const queue = [from];
  while (queue.length) {
    const id = queue.shift()!;
    for (const edge of graph.edges) {
      if (edge.from === id && !seen.has(edge.to)) {
        seen.add(edge.to);
        queue.push(edge.to);
      }
    }
  }
  return seen;
}

function hasCycle(graph: AutomationGraph): boolean {
  const colors = new Map<string, number>();
  const visit = (id: string): boolean => {
    if (colors.get(id) === 1) return true;
    if (colors.get(id) === 2) return false;
    colors.set(id, 1);
    for (const edge of graph.edges) if (edge.from === id && visit(edge.to)) return true;
    colors.set(id, 2);
    return false;
  };
  return graph.nodes.some((node) => visit(node.id));
}

function idAllocator(graph: AutomationGraph, ids: IdFactory): () => string {
  const used = new Set([...graph.nodes.map((node) => node.id), ...graph.edges.map((edge) => edge.id)]);
  return () => {
    for (let attempt = 0; attempt < 100; attempt++) {
      const id = safeId(ids());
      if (!used.has(id)) {
        used.add(id);
        return id;
      }
    }
    throw new Error('Could not generate a unique graph ID.');
  };
}

function safeId(value: string): string {
  const id = value.toLowerCase().replace(/[^a-z0-9_-]/g, '-').slice(0, 64);
  if (!id) throw new Error('ID factory returned an invalid ID.');
  return id;
}

function append(map: Map<string, AutomationEdge[]>, id: string, edge: AutomationEdge): void {
  map.set(id, [...(map.get(id) ?? []), edge]);
}

function nodeIssue(issues: AutomationIssue[], code: string, nodeId: string, message: string): void {
  issues.push({ code, node_id: nodeId, message });
}

function edgeIssue(issues: AutomationIssue[], code: string, edgeId: string, message: string): void {
  issues.push({ code, edge_id: edgeId, message });
}

function defaultName(kind: AutomationNodeKind): string {
  const names: Record<AutomationNodeKind, string> = {
    trigger: 'Trigger',
    send_email: 'Send email',
    wait: 'Wait',
    condition: 'Condition',
    add_tag: 'Add tag',
    remove_tag: 'Remove tag',
    exit: 'Exit',
  };
  return names[kind];
}

function formatDuration(seconds: number): string {
  if (seconds % 86_400 === 0) return `${seconds / 86_400} day${seconds === 86_400 ? '' : 's'}`;
  if (seconds % 3_600 === 0) return `${seconds / 3_600} hour${seconds === 3_600 ? '' : 's'}`;
  return `${Math.max(1, Math.round(seconds / 60))} minutes`;
}

function nonempty(value: unknown): boolean {
  return typeof value === 'string' && value.trim().length > 0;
}

function validUuid(value: unknown): boolean {
  return typeof value === 'string' && uuidPattern.test(value) && value !== '00000000-0000-0000-0000-000000000000';
}

function validTimeZone(value: unknown): boolean {
  if (!nonempty(value)) return false;
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: String(value) }).format();
    return true;
  } catch {
    return false;
  }
}

function label(value: unknown, max: number): boolean {
  return nonempty(value) && String(value).trim().length <= max;
}

function keysAre(value: Record<string, unknown>, allowed: string[]): boolean {
  return Object.keys(value).every((key) => allowed.includes(key));
}

function validHttpUrl(value: string): boolean {
  try {
    const url = new URL(value);
    return url.protocol === 'http:' || url.protocol === 'https:';
  } catch {
    return false;
  }
}
