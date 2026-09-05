import { Component, ElementRef, EventEmitter, Input, Output, ViewChild } from '@angular/core';
import {
  AutomationGraph,
  AutomationIssue,
  AutomationNode,
  AutomationNodeKind,
  NodeStats,
} from '../../../../core/automations.service';
import { deleteNode, GraphReferences, insertNode, summarizeNode } from './graph-ops';
import { nodeCompletedCount } from './node-stats';
import { GraphLayout, layoutGraph } from './layout';

@Component({
  selector: 'app-automation-canvas',
  standalone: true,
  template: `
    <section class="canvas-shell" data-testid="automation-canvas-shell">
      <div class="controls" aria-label="Canvas controls">
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="canvas-zoom-out" (click)="zoomBy(0.8)">−</button>
        <span>{{ (zoom * 100).toFixed(0) }}%</span>
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="canvas-zoom-in" (click)="zoomBy(1.25)">+</button>
        <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="canvas-fit" (click)="fit()">Fit</button>
      </div>
      <div
        #viewport
        class="viewport"
        role="application"
        aria-label="Automation workflow canvas"
        data-testid="automation-canvas"
        (pointerdown)="startPan($event)"
        (pointermove)="movePan($event)"
        (pointerup)="endPan($event)"
        (pointercancel)="endPan($event)"
        (wheel)="onWheel($event)"
        (keydown)="onKeydown($event)"
      >
        <div class="world" [style.width.px]="layout.width" [style.height.px]="layout.height" [style.transform]="worldTransform">
          <svg class="edges" [attr.width]="layout.width" [attr.height]="layout.height" aria-hidden="true">
            @for (edge of layout.edges; track edge.id) {
              <path [attr.d]="edge.path" />
              @if (edge.label; as label) { <text [attr.x]="label.x" [attr.y]="label.y">{{ label.text }}</text> }
            }
          </svg>
          @for (edge of layout.edges; track edge.id) {
            @if (!readOnly) {
              <button
                type="button"
                class="edge-plus"
                data-testid="edge-plus"
                [attr.data-edge-id]="edge.id"
                [style.left.px]="edge.plus.x"
                [style.top.px]="edge.plus.y"
                [attr.aria-label]="'Add step on edge ' + edge.id"
                (pointerdown)="$event.stopPropagation()"
                (click)="openPicker(edge.id, $event)"
              >+</button>
            }
          }
          @for (placed of layout.nodes; track placed.id) {
            @if (node(placed.id); as item) {
              <button
                type="button"
                class="node"
                data-testid="canvas-node"
                [attr.data-node-id]="item.id"
                [attr.data-kind]="item.kind"
                [attr.data-invalid]="isInvalid(item.id)"
                [attr.aria-selected]="selectedId === item.id"
                [attr.tabindex]="selectedId === item.id ? 0 : -1"
                [title]="errorTitle(item.id)"
                [style.left.px]="placed.x"
                [style.top.px]="placed.y"
                [style.width.px]="placed.w"
                [style.height.px]="placed.h"
                (pointerdown)="$event.stopPropagation()"
                (click)="selectNode(item.id)"
                (dblclick)="openNode.emit(item.id)"
              >
                <span class="kind">{{ icon(item.kind) }} {{ item.name || kindLabel(item.kind) }}</span>
                <span class="summary">{{ summarize(item) }}</span>
              </button>
              @if (stats) {
                <div
                  class="node-stats"
                  data-testid="node-stats"
                  [style.left.px]="placed.x"
                  [style.top.px]="placed.y + placed.h + 4"
                  [style.width.px]="placed.w"
                >
                  <span class="mf-pill mf-pill-neutral" data-testid="node-stat-entered">entered {{ statsFor(item.id)?.entered ?? 0 }}</span>
                  <span class="mf-pill mf-pill-neutral" data-testid="node-stat-completed">completed {{ completedCount(item.id) }}</span>
                  @if (item.kind === 'send_email') {
                    <span class="mf-pill mf-pill-accent" data-testid="node-stat-sent">sent {{ statsFor(item.id)?.sent ?? 0 }}</span>
                    <span class="mf-pill mf-pill-accent" data-testid="node-stat-opened">opened {{ statsFor(item.id)?.opened ?? 0 }}</span>
                    <span class="mf-pill mf-pill-accent" data-testid="node-stat-clicked">clicked {{ statsFor(item.id)?.clicked ?? 0 }}</span>
                  }
                </div>
              }
            }
          }
          @if (pickerEdgeId) {
            <div class="picker" data-testid="node-picker" [style.left.px]="pickerPosition.x" [style.top.px]="pickerPosition.y" (pointerdown)="$event.stopPropagation()">
              <p>Add a step</p>
              @for (kind of insertKinds; track kind) {
                <button type="button" class="picker-item" [attr.data-testid]="'insert-' + kind" (click)="add(kind)">{{ icon(kind) }} {{ kindLabel(kind) }}</button>
              }
              <button type="button" class="picker-close" data-testid="node-picker-close" (click)="pickerEdgeId = null">Cancel</button>
            </div>
          }
        </div>
      </div>
    </section>
  `,
  styles: [`
    :host{display:block;min-width:0;height:100%}.canvas-shell{position:relative;height:100%;min-height:520px;background:var(--mf-surface-inset);overflow:hidden}.controls{position:absolute;z-index:5;top:12px;left:12px;display:flex;align-items:center;gap:6px;padding:5px;border:1px solid var(--mf-border);border-radius:var(--mf-radius-sm);background:var(--mf-surface);box-shadow:var(--mf-shadow-sm);font-size:var(--mf-fs-xs)}.viewport{position:absolute;inset:0;overflow:hidden;outline:none;cursor:grab;background-image:radial-gradient(var(--mf-border) 1px,transparent 1px);background-size:20px 20px}.viewport.panning{cursor:grabbing}.world{position:absolute;transform-origin:0 0}.edges{position:absolute;inset:0;overflow:visible;pointer-events:none}.edges path{fill:none;stroke:var(--mf-border-strong);stroke-width:2}.edges text{fill:var(--mf-text-muted);font-size:12px;font-weight:700;text-anchor:middle}.node{position:absolute;display:flex;flex-direction:column;align-items:flex-start;justify-content:center;gap:5px;padding:12px 14px;text-align:left;border:1px solid var(--mf-border-strong);border-radius:var(--mf-radius);color:var(--mf-text);background:var(--mf-surface);box-shadow:var(--mf-shadow-sm);cursor:pointer}.node:hover,.node[aria-selected="true"]{border-color:var(--mf-accent);box-shadow:var(--mf-ring)}.node[data-invalid="true"]{border-color:var(--mf-danger);box-shadow:0 0 0 2px var(--mf-danger-soft)}.kind{font-weight:700;font-size:var(--mf-fs-sm)}.summary{max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--mf-text-muted);font-size:var(--mf-fs-xs)}.edge-plus{position:absolute;z-index:2;width:26px;height:26px;padding:0;transform:translate(-50%,-50%);border:1px solid var(--mf-accent);border-radius:50%;color:var(--mf-accent-text);background:var(--mf-surface);font-size:18px;line-height:22px;cursor:pointer}.picker{position:absolute;z-index:10;width:190px;padding:10px;transform:translate(-50%,8px);border:1px solid var(--mf-border);border-radius:var(--mf-radius);background:var(--mf-surface);box-shadow:var(--mf-shadow)}.picker p{margin:0 0 6px;font-size:var(--mf-fs-xs);font-weight:700;color:var(--mf-text-muted);text-transform:uppercase}.picker-item,.picker-close{display:block;width:100%;padding:7px 8px;text-align:left;border:0;border-radius:var(--mf-radius-sm);color:var(--mf-text);background:transparent;cursor:pointer}.picker-item:hover{background:var(--mf-accent-soft)}.picker-close{margin-top:4px;color:var(--mf-text-muted)}.node-stats{position:absolute;display:flex;gap:4px;pointer-events:none}.node-stats .mf-pill{font-size:10px;padding:2px 6px;white-space:nowrap}
  `],
})
export class AutomationCanvasComponent {
  private graphValue: AutomationGraph = { nodes: [], edges: [] };
  private layoutValue: GraphLayout = layoutGraph(this.graphValue);
  @Input({ required: true })
  set graph(value: AutomationGraph) {
    if (value === this.graphValue) return;
    this.graphValue = value;
    this.layoutValue = layoutGraph(value);
  }
  get graph(): AutomationGraph { return this.graphValue; }
  @Input() selectedId: string | null = null;
  @Input() issues: AutomationIssue[] = [];
  @Input() readOnly = false;
  @Input() references: GraphReferences = {};
  @Input() stats: Record<string, NodeStats> | null = null;
  @Output() graphChange = new EventEmitter<AutomationGraph>();
  @Output() selectedIdChange = new EventEmitter<string | null>();
  @Output() openNode = new EventEmitter<string>();
  @ViewChild('viewport') viewport?: ElementRef<HTMLElement>;

  readonly insertKinds: Array<Exclude<AutomationNodeKind, 'trigger'>> = ['send_email', 'wait', 'condition', 'add_tag', 'remove_tag', 'exit'];
  readonly summarize = summarizeNode;
  zoom = 1;
  panX = 24;
  panY = 24;
  pickerEdgeId: string | null = null;
  pickerPosition = { x: 0, y: 0 };
  private panStart: { pointerId: number; x: number; y: number; panX: number; panY: number } | null = null;

  get layout(): GraphLayout { return this.layoutValue; }
  get worldTransform(): string { return `translate(${this.panX}px, ${this.panY}px) scale(${this.zoom})`; }
  node(id: string): AutomationNode | undefined { return this.graph.nodes.find((node) => node.id === id); }
  isInvalid(id: string): boolean { return this.issues.some((issue) => issue.node_id === id); }
  errorTitle(id: string): string { const messages = this.issues.filter((issue) => issue.node_id === id).map((issue) => issue.message); return messages.length ? messages.join('\n') : ''; }
  kindLabel(kind: AutomationNodeKind): string { return kind.replace('_', ' ').replace(/^./, (letter) => letter.toUpperCase()); }
  statsFor(id: string): NodeStats | undefined { return this.stats?.[id]; }
  completedCount(id: string): number { const value = this.stats?.[id]; return value ? nodeCompletedCount(value) : 0; }
  icon(kind: AutomationNodeKind): string { return ({ trigger: '⚡', send_email: '✉', wait: '◷', condition: '◇', add_tag: '+', remove_tag: '−', exit: '■' } as Record<AutomationNodeKind, string>)[kind]; }

  selectNode(id: string): void {
    this.selectedIdChange.emit(id);
    this.openNode.emit(id);
  }
  openPicker(edgeId: string, event: MouseEvent): void {
    event.stopPropagation();
    const edge = this.layout.edges.find((candidate) => candidate.id === edgeId);
    if (!edge) return;
    this.pickerEdgeId = edgeId;
    this.pickerPosition = edge.plus;
  }
  add(kind: Exclude<AutomationNodeKind, 'trigger'>): void {
    if (!this.pickerEdgeId) return;
    const result = insertNode(this.graph, this.pickerEdgeId, kind, this.references);
    this.pickerEdgeId = null;
    this.graphChange.emit(result.graph);
    this.selectedIdChange.emit(result.selectedId);
    if (result.selectedId) this.openNode.emit(result.selectedId);
  }
  removeSelected(): void {
    if (!this.selectedId || this.readOnly) return;
    try {
      const result = deleteNode(this.graph, this.selectedId);
      if (result.removedIds.length > 1 && !globalThis.confirm(`Delete this condition and ${result.removedIds.length - 1} step(s) used only by its No branch?`)) return;
      this.graphChange.emit(result.graph);
      this.selectedIdChange.emit(result.selectedId);
    } catch { /* protected nodes remain in place */ }
  }

  startPan(event: PointerEvent): void {
    const target = event.target as Element | null;
    if (target?.closest('button, input, select, textarea, a')) return;
    if (!this.viewport) return;
    this.pickerEdgeId = null;
    this.panStart = { pointerId: event.pointerId, x: event.clientX, y: event.clientY, panX: this.panX, panY: this.panY };
    this.viewport.nativeElement.setPointerCapture(event.pointerId);
  }
  movePan(event: PointerEvent): void {
    if (!this.panStart || this.panStart.pointerId !== event.pointerId) return;
    this.panX = this.panStart.panX + event.clientX - this.panStart.x;
    this.panY = this.panStart.panY + event.clientY - this.panStart.y;
  }
  endPan(event: PointerEvent): void {
    if (this.panStart?.pointerId === event.pointerId) this.panStart = null;
  }
  onWheel(event: WheelEvent): void {
    event.preventDefault();
    if (!event.ctrlKey && !event.metaKey) {
      this.panX -= event.deltaX;
      this.panY -= event.deltaY;
      return;
    }
    const rect = this.viewport?.nativeElement.getBoundingClientRect();
    if (!rect) return;
    const oldZoom = this.zoom;
    const next = clamp(oldZoom * Math.exp(-event.deltaY * 0.002), 0.25, 2);
    const cursorX = event.clientX - rect.left;
    const cursorY = event.clientY - rect.top;
    this.panX = cursorX - ((cursorX - this.panX) / oldZoom) * next;
    this.panY = cursorY - ((cursorY - this.panY) / oldZoom) * next;
    this.zoom = next;
  }
  zoomBy(factor: number): void { this.zoom = clamp(this.zoom * factor, 0.25, 2); }
  fit(): void {
    const element = this.viewport?.nativeElement;
    if (!element || !this.layout.width || !this.layout.height) return;
    this.zoom = clamp(Math.min((element.clientWidth - 48) / this.layout.width, (element.clientHeight - 48) / this.layout.height), 0.25, 1);
    this.panX = (element.clientWidth - this.layout.width * this.zoom) / 2;
    this.panY = 24;
  }
  onKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') { this.pickerEdgeId = null; this.openNode.emit(''); return; }
    if (event.key === 'Delete' || event.key === 'Backspace') { event.preventDefault(); this.removeSelected(); return; }
    if (event.key === 'Enter' && this.selectedId) { this.openNode.emit(this.selectedId); return; }
    if (!this.selectedId || !['ArrowDown', 'ArrowUp', 'ArrowLeft', 'ArrowRight'].includes(event.key)) return;
    const target = this.keyboardTarget(event.key);
    if (!target) return;
    event.preventDefault();
    this.selectedIdChange.emit(target);
    setTimeout(() => this.viewport?.nativeElement.querySelector<HTMLElement>(`[data-node-id="${target}"]`)?.focus());
  }

  private keyboardTarget(key: string): string | null {
    const current = this.layout.nodes.find((node) => node.id === this.selectedId);
    if (!current) return null;
    if (key === 'ArrowDown') return this.graph.edges.filter((edge) => edge.from === current.id).sort((a, b) => branchOrder(a.branch) - branchOrder(b.branch) || a.id.localeCompare(b.id))[0]?.to ?? null;
    if (key === 'ArrowUp') return this.graph.edges.filter((edge) => edge.to === current.id).sort((a, b) => a.id.localeCompare(b.id))[0]?.from ?? null;
    const row = this.layout.nodes.filter((node) => node.rank === current.rank).sort((a, b) => a.x - b.x);
    const index = row.findIndex((node) => node.id === current.id);
    return key === 'ArrowLeft' ? row[index - 1]?.id ?? null : row[index + 1]?.id ?? null;
  }
}

function clamp(value: number, min: number, max: number): number { return Math.max(min, Math.min(max, value)); }
function branchOrder(branch: 'yes' | 'no' | null): number { return branch === 'yes' ? 0 : branch === 'no' ? 1 : 2; }
