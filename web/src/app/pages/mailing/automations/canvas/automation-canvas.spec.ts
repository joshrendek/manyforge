import { TestBed } from '@angular/core/testing';
import { AutomationGraph, NodeStats } from '../../../../core/automations.service';
import { AutomationCanvasComponent } from './automation-canvas';

const graph: AutomationGraph = {
  nodes: [
    { id: 'trigger', kind: 'trigger', config: { type: 'list_joined', list_id: '11111111-1111-4111-8111-111111111111' } },
    { id: 'exit', kind: 'exit', config: {} },
  ],
  edges: [{ id: 'edge', from: 'trigger', to: 'exit', branch: null }],
};

describe('AutomationCanvasComponent', () => {
  it('renders HTML nodes and inserts from a real edge button', () => {
    vi.stubGlobal('crypto', { randomUUID: vi.fn().mockReturnValueOnce('wait').mockReturnValueOnce('before').mockReturnValueOnce('after') });
    const fixture = TestBed.createComponent(AutomationCanvasComponent);
    fixture.componentInstance.graph = graph;
    let changed: AutomationGraph | undefined;
    fixture.componentInstance.graphChange.subscribe((value) => changed = value);
    fixture.detectChanges();
    expect(fixture.nativeElement.querySelectorAll('[data-testid="canvas-node"]')).toHaveLength(2);
    (fixture.nativeElement.querySelector('[data-testid="edge-plus"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    (fixture.nativeElement.querySelector('[data-testid="insert-wait"]') as HTMLButtonElement).click();
    expect(changed?.nodes.some((node) => node.kind === 'wait')).toBe(true);
    vi.unstubAllGlobals();
  });

  it('navigates down to the first outgoing node', () => {
    const fixture = TestBed.createComponent(AutomationCanvasComponent);
    fixture.componentInstance.graph = graph;
    fixture.componentInstance.selectedId = 'trigger';
    let selected: string | null = null;
    fixture.componentInstance.selectedIdChange.subscribe((value) => selected = value);
    fixture.detectChanges();
    fixture.componentInstance.onKeydown(new KeyboardEvent('keydown', { key: 'ArrowDown' }));
    expect(selected).toBe('exit');
  });

  it('renders per-node stat pills only when stats are provided', () => {
    const graph: AutomationGraph = {
      nodes: [
        { id: 'trigger', kind: 'trigger', config: { type: 'list_joined', list_id: '11111111-1111-4111-8111-111111111111' } },
        { id: 'send', kind: 'send_email', config: { template_id: '22222222-2222-4222-8222-222222222222', track_opens: true, track_clicks: true } },
      ],
      edges: [{ id: 'edge', from: 'trigger', to: 'send', branch: null }],
    };
    const stats: Record<string, NodeStats> = {
      trigger: {
        node_id: 'trigger', node_kind: 'trigger', entered: 4, waiting: 0, advanced: 4,
        sent: 0, opened: 0, clicked: 0, branch_yes: 0, branch_no: 0, exited: 0, errors: 0,
      },
      send: {
        node_id: 'send', node_kind: 'send_email', entered: 4, waiting: 0, advanced: 2,
        sent: 4, opened: 1, clicked: 1, branch_yes: 0, branch_no: 0, exited: 0, errors: 0,
      },
    };

    const withoutStats = TestBed.createComponent(AutomationCanvasComponent);
    withoutStats.componentInstance.graph = graph;
    withoutStats.detectChanges();
    expect(withoutStats.nativeElement.querySelectorAll('[data-testid="node-stats"]')).toHaveLength(0);
    withoutStats.destroy();

    const withStats = TestBed.createComponent(AutomationCanvasComponent);
    withStats.componentInstance.graph = graph;
    withStats.componentInstance.stats = stats;
    withStats.detectChanges();

    // The stats bar is the direct sibling of its node button.
    const triggerStats = withStats.nativeElement.querySelector('[data-node-id="trigger"] + [data-testid="node-stats"]');
    expect(triggerStats?.querySelector('[data-testid="node-stat-entered"]')?.textContent).toContain('4');
    expect(triggerStats?.querySelector('[data-testid="node-stat-completed"]')?.textContent).toContain('4');
    const sendStats = withStats.nativeElement.querySelector('[data-node-id="send"] + [data-testid="node-stats"]');
    // completed = advanced + sent + branch_yes + branch_no + exited
    expect(sendStats?.querySelector('[data-testid="node-stat-entered"]')?.textContent).toContain('4');
    expect(sendStats?.querySelector('[data-testid="node-stat-completed"]')?.textContent).toContain('6');
    expect(sendStats?.querySelector('[data-testid="node-stat-sent"]')?.textContent).toContain('4');
    expect(sendStats?.querySelector('[data-testid="node-stat-opened"]')?.textContent).toContain('1');
    expect(sendStats?.querySelector('[data-testid="node-stat-clicked"]')?.textContent).toContain('1');

    // non-send nodes do not show sent/opened/clicked
    expect(triggerStats?.querySelector('[data-testid="node-stat-sent"]')).toBeNull();
    expect(triggerStats?.querySelector('[data-testid="node-stat-opened"]')).toBeNull();
    expect(triggerStats?.querySelector('[data-testid="node-stat-clicked"]')).toBeNull();
  });
});
