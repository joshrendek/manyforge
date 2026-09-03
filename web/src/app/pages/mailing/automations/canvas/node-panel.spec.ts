import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { AutomationGraph } from '../../../../core/automations.service';
import { AutomationNodePanelComponent } from './node-panel';

describe('AutomationNodePanelComponent', () => {
  it('emits immutable configuration edits', () => {
    const graph: AutomationGraph = { nodes: [{ id: 'wait', kind: 'wait', config: { mode: 'duration', seconds: 60 } }], edges: [] };
    const fixture = TestBed.createComponent(AutomationNodePanelComponent);
    fixture.componentInstance.graph = graph;
    fixture.componentInstance.selectedId = 'wait';
    let changed: AutomationGraph | undefined;
    fixture.componentInstance.graphChange.subscribe((value) => changed = value);
    fixture.detectChanges();
    fixture.componentInstance.patchConfig({ seconds: 120 });
    expect(changed?.nodes[0].config).toEqual({ mode: 'duration', seconds: 120 });
    expect(graph.nodes[0].config).toEqual({ mode: 'duration', seconds: 60 });
  });

  it('selects the merge target after retargeting an exit', () => {
    const graph: AutomationGraph = {
      nodes: [
        { id: 'trigger', kind: 'trigger', config: { type: 'list_joined', list_id: '11111111-1111-4111-8111-111111111111' } },
        { id: 'exit', kind: 'exit', config: {} },
        { id: 'target', kind: 'wait', config: { mode: 'duration', seconds: 60 } },
      ],
      edges: [{ id: 'edge', from: 'trigger', to: 'exit', branch: null }],
    };
    const fixture = TestBed.createComponent(AutomationNodePanelComponent);
    fixture.componentInstance.graph = graph;
    fixture.componentInstance.selectedId = 'exit';
    fixture.componentInstance.continueTarget = 'target';
    let selected: string | null = null;
    fixture.componentInstance.selectedIdChange.subscribe((value) => selected = value);
    fixture.componentInstance.continueTo();
    expect(selected).toBe('target');
  });
});
