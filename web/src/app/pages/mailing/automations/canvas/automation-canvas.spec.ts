import { TestBed } from '@angular/core/testing';
import { describe, expect, it, vi } from 'vitest';
import { AutomationGraph } from '../../../../core/automations.service';
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
});
