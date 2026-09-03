import { Component, EventEmitter, Input, Output } from '@angular/core';
import { FormsModule } from '@angular/forms';
import {
  AutomationGraph,
  AutomationIssue,
  AutomationNode,
  AutomationNodeConfig,
  ConditionConfig,
  ConditionPredicate,
  SendEmailConfig,
  TriggerConfig,
  WaitConfig,
} from '../../../../core/automations.service';
import { MailingList, MailingTemplate } from '../../../../core/mailing.service';
import { continuationTargets, retargetExit } from './graph-ops';

@Component({
  selector: 'app-automation-node-panel',
  standalone: true,
  imports: [FormsModule],
  template: `
    <aside class="panel" data-testid="automation-node-panel" aria-label="Step settings">
      @if (node; as current) {
        <div class="panel-head">
          <div>
            <p class="eyebrow">{{ kindLabel(current.kind) }}</p>
            <h2>{{ current.name || kindLabel(current.kind) }}</h2>
          </div>
          <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="node-panel-close" (click)="close.emit()">Close</button>
        </div>

        <div class="mf-field">
          <label for="automation-node-name">Step name</label>
          <input id="automation-node-name" class="mf-input" data-testid="automation-node-name" [disabled]="readOnly" [ngModel]="current.name ?? ''" (ngModelChange)="updateName($event)" />
        </div>

        @switch (current.kind) {
          @case ('trigger') {
            <div class="mf-field">
              <label for="trigger-type">Trigger</label>
              <select id="trigger-type" class="mf-select" data-testid="trigger-type" [disabled]="readOnly" [ngModel]="trigger.type" (ngModelChange)="setTriggerType($event)">
                <option value="list_joined">Joins a list</option>
                <option value="tag_added">Tag is added</option>
                <option value="event">Event is received</option>
              </select>
            </div>
            <div class="mf-field">
              <label for="trigger-list">Mailing list</label>
              <select id="trigger-list" class="mf-select" data-testid="trigger-list" [disabled]="readOnly" [ngModel]="trigger.list_id" (ngModelChange)="patchConfig({ list_id: $event })">
                <option value="" disabled>Choose a list…</option>
                @for (list of lists; track list.id) { <option [value]="list.id">{{ list.name }}</option> }
              </select>
            </div>
            @if (trigger.type === 'tag_added') {
              <div class="mf-field"><label for="trigger-tag">Tag</label><input id="trigger-tag" class="mf-input" data-testid="trigger-tag" [disabled]="readOnly" [ngModel]="trigger.tag" (ngModelChange)="patchConfig({ tag: $event })" /></div>
            }
            @if (trigger.type === 'event') {
              <div class="mf-field"><label for="trigger-event">Event name</label><input id="trigger-event" class="mf-input" data-testid="trigger-event" [disabled]="readOnly" [ngModel]="trigger.name" (ngModelChange)="patchConfig({ name: $event })" /></div>
            }
          }
          @case ('send_email') {
            <div class="mf-field">
              <label for="send-template">Template</label>
              <select id="send-template" class="mf-select" data-testid="send-template" [disabled]="readOnly" [ngModel]="send.template_id" (ngModelChange)="patchConfig({ template_id: $event })">
                <option value="" disabled>Choose a template…</option>
                @for (template of templates; track template.id) { <option [value]="template.id">{{ template.name }}</option> }
              </select>
            </div>
            <label class="check"><input type="checkbox" data-testid="send-track-opens" [disabled]="readOnly" [ngModel]="send.track_opens" (ngModelChange)="patchConfig({ track_opens: $event })" /> Track opens</label>
            <label class="check"><input type="checkbox" data-testid="send-track-clicks" [disabled]="readOnly" [ngModel]="send.track_clicks" (ngModelChange)="patchConfig({ track_clicks: $event })" /> Track clicks</label>
          }
          @case ('wait') {
            <div class="mf-field"><label for="wait-mode">Wait</label><select id="wait-mode" class="mf-select" data-testid="wait-mode" [disabled]="readOnly" [ngModel]="wait.mode" (ngModelChange)="setWaitMode($event)"><option value="duration">For a duration</option><option value="until">Until a time</option></select></div>
            @if (wait.mode === 'duration') {
              <div class="mf-field"><label for="wait-seconds">Seconds</label><input id="wait-seconds" type="number" min="60" max="31536000" class="mf-input" data-testid="wait-seconds" [disabled]="readOnly" [ngModel]="wait.seconds" (ngModelChange)="patchConfig({ seconds: +$event })" /></div>
            } @else {
              <div class="mf-field"><label for="wait-weekday">Weekday (optional, 1–7)</label><input id="wait-weekday" type="number" min="1" max="7" class="mf-input" data-testid="wait-weekday" [disabled]="readOnly" [ngModel]="wait.weekday" (ngModelChange)="patchUntilWeekday($event)" /></div>
              <div class="mf-field"><label for="wait-time">Time</label><input id="wait-time" type="time" class="mf-input" data-testid="wait-time" [disabled]="readOnly" [ngModel]="wait.time" (ngModelChange)="patchConfig({ time: $event })" /></div>
              <div class="mf-field"><label for="wait-timezone">Timezone</label><input id="wait-timezone" class="mf-input" data-testid="wait-timezone" [disabled]="readOnly" [ngModel]="wait.timezone" (ngModelChange)="patchConfig({ timezone: $event })" /></div>
            }
          }
          @case ('condition') {
            <div class="mf-field"><label for="condition-type">Condition</label><select id="condition-type" class="mf-select" data-testid="condition-type" [disabled]="readOnly" [ngModel]="predicate.type" (ngModelChange)="setPredicateType($event)"><option value="opened_email">Opened an email</option><option value="clicked_link">Clicked a link</option><option value="has_tag">Has a tag</option><option value="on_list">Is on a list</option><option value="event_received">Event was received</option></select></div>
            @if (predicate.type === 'opened_email' || predicate.type === 'clicked_link') {
              <div class="mf-field"><label for="condition-email">Earlier email step</label><select id="condition-email" class="mf-select" data-testid="condition-email" [disabled]="readOnly" [ngModel]="predicate.node_id" (ngModelChange)="patchPredicate({ node_id: $event })"><option value="" disabled>Choose an email…</option>@for (email of earlierEmails; track email.id) { <option [value]="email.id">{{ email.name || 'Send email' }}</option> }</select></div>
              @if (predicate.type === 'clicked_link') { <div class="mf-field"><label for="condition-url">URL (optional)</label><input id="condition-url" class="mf-input" data-testid="condition-url" [disabled]="readOnly" [ngModel]="predicate.url ?? ''" (ngModelChange)="patchPredicate({ url: $event || null })" /></div> }
            }
            @if (predicate.type === 'has_tag') { <div class="mf-field"><label for="condition-tag">Tag</label><input id="condition-tag" class="mf-input" data-testid="condition-tag" [disabled]="readOnly" [ngModel]="predicate.tag" (ngModelChange)="patchPredicate({ tag: $event })" /></div> }
            @if (predicate.type === 'on_list') { <div class="mf-field"><label for="condition-list">Mailing list</label><select id="condition-list" class="mf-select" data-testid="condition-list" [disabled]="readOnly" [ngModel]="predicate.list_id" (ngModelChange)="patchPredicate({ list_id: $event })"><option value="" disabled>Choose a list…</option>@for (list of lists; track list.id) { <option [value]="list.id">{{ list.name }}</option> }</select></div> }
            @if (predicate.type === 'event_received') { <div class="mf-field"><label for="condition-event">Event name</label><input id="condition-event" class="mf-input" data-testid="condition-event" [disabled]="readOnly" [ngModel]="predicate.name" (ngModelChange)="patchPredicate({ name: $event })" /></div><div class="mf-field"><label for="condition-window">Within seconds (optional)</label><input id="condition-window" type="number" class="mf-input" data-testid="condition-window" [disabled]="readOnly" [ngModel]="predicate.within_seconds" (ngModelChange)="patchPredicate({ within_seconds: $event ? +$event : null })" /></div> }
          }
          @case ('add_tag') { <div class="mf-field"><label for="add-tag">Tag</label><input id="add-tag" class="mf-input" data-testid="node-tag" [disabled]="readOnly" [ngModel]="tag" (ngModelChange)="patchConfig({ tag: $event })" /></div> }
          @case ('remove_tag') { <div class="mf-field"><label for="remove-tag">Tag</label><input id="remove-tag" class="mf-input" data-testid="node-tag" [disabled]="readOnly" [ngModel]="tag" (ngModelChange)="patchConfig({ tag: $event })" /></div> }
          @case ('exit') {
            <p class="mf-hint">This path ends here.</p>
            @if (!readOnly && targets.length) {
              <div class="mf-field"><label for="continue-target">Instead, continue to…</label><select id="continue-target" class="mf-select" data-testid="continue-target" [(ngModel)]="continueTarget"><option value="">Choose a step…</option>@for (target of targets; track target.id) { <option [value]="target.id">{{ target.name || kindLabel(target.kind) }}</option> }</select></div>
              <button type="button" class="mf-btn mf-btn-secondary mf-btn-sm" data-testid="continue-apply" [disabled]="!continueTarget" (click)="continueTo()">Continue there</button>
            }
          }
        }

        @if (nodeIssues.length) {
          <div class="issues" data-testid="node-errors"><strong>Fix before saving</strong><ul>@for (issue of nodeIssues; track $index) { <li>{{ issue.message }}</li> }</ul></div>
        }
        @if (!readOnly && current.kind !== 'trigger') {
          <button type="button" class="mf-btn mf-btn-danger mf-btn-sm delete" data-testid="automation-node-delete" (click)="deleteRequested.emit(current.id)">Delete step</button>
        }
      }
    </aside>
  `,
  styles: [`
    :host{display:block;height:100%}.panel{box-sizing:border-box;width:340px;height:100%;overflow:auto;padding:20px;border-left:1px solid var(--mf-border);background:var(--mf-surface)}
    .panel-head{display:flex;justify-content:space-between;gap:12px;align-items:flex-start;margin-bottom:18px}.panel-head h2{margin:2px 0 0;font-size:var(--mf-fs-lg)}.eyebrow{margin:0;color:var(--mf-text-muted);font-size:var(--mf-fs-xs);font-weight:700;text-transform:uppercase}.mf-field{margin-bottom:14px}.check{display:flex;gap:8px;align-items:center;margin:12px 0;font-size:var(--mf-fs-sm)}.issues{margin-top:18px;padding:12px;border-radius:var(--mf-radius-sm);color:var(--mf-danger-text);background:var(--mf-danger-soft);font-size:var(--mf-fs-sm)}.issues ul{margin:6px 0 0;padding-left:18px}.delete{margin-top:20px}
  `],
})
export class AutomationNodePanelComponent {
  @Input({ required: true }) graph!: AutomationGraph;
  @Input() selectedId: string | null = null;
  @Input() lists: MailingList[] = [];
  @Input() templates: MailingTemplate[] = [];
  @Input() issues: AutomationIssue[] = [];
  @Input() readOnly = false;
  @Output() graphChange = new EventEmitter<AutomationGraph>();
  @Output() selectedIdChange = new EventEmitter<string | null>();
  @Output() close = new EventEmitter<void>();
  @Output() deleteRequested = new EventEmitter<string>();
  continueTarget = '';

  get node(): AutomationNode | undefined { return this.graph.nodes.find((node) => node.id === this.selectedId); }
  get trigger(): TriggerConfig { return this.node!.config as TriggerConfig; }
  get send(): SendEmailConfig { return this.node!.config as SendEmailConfig; }
  get wait(): WaitConfig { return this.node!.config as WaitConfig; }
  get predicate(): ConditionPredicate { return (this.node!.config as ConditionConfig).predicate; }
  get tag(): string { return (this.node!.config as { tag: string }).tag; }
  get nodeIssues(): AutomationIssue[] { return this.issues.filter((issue) => issue.node_id === this.selectedId); }
  get targets(): AutomationNode[] { return this.node ? continuationTargets(this.graph, this.node.id) : []; }
  get earlierEmails(): AutomationNode[] {
    if (!this.node) return [];
    return this.graph.nodes.filter((candidate) => candidate.kind === 'send_email' && this.canReach(candidate.id, this.node!.id));
  }

  updateName(name: string): void { this.updateNode({ name }); }
  patchConfig(patch: Record<string, unknown>): void { this.updateNode({ config: { ...(this.node!.config as object), ...patch } as AutomationNodeConfig }); }
  patchPredicate(patch: Record<string, unknown>): void { this.patchConfig({ predicate: { ...this.predicate, ...patch } }); }
  patchUntilWeekday(value: number | string | null): void {
    const config = { ...(this.node!.config as Record<string, unknown>) };
    if (value === '' || value === null) config['weekday'] = null; else config['weekday'] = +value;
    this.updateNode({ config: config as AutomationNodeConfig });
  }
  setTriggerType(type: TriggerConfig['type']): void {
    const list_id = this.trigger.list_id;
    this.updateNode({ config: type === 'tag_added' ? { type, list_id, tag: '' } : type === 'event' ? { type, list_id, name: '' } : { type, list_id } });
  }
  setWaitMode(mode: WaitConfig['mode']): void {
    this.updateNode({ config: mode === 'duration' ? { mode, seconds: 3600 } : { mode, time: '09:00', timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC' } });
  }
  setPredicateType(type: ConditionPredicate['type']): void {
    const firstEmail = this.earlierEmails[0]?.id ?? '';
    const predicate: ConditionPredicate = type === 'opened_email' ? { type, node_id: firstEmail } : type === 'clicked_link' ? { type, node_id: firstEmail, url: null } : type === 'has_tag' ? { type, tag: '' } : type === 'on_list' ? { type, list_id: this.lists[0]?.id ?? '' } : { type, name: '', within_seconds: null };
    this.updateNode({ config: { predicate } });
  }
  continueTo(): void {
    if (!this.node || !this.continueTarget) return;
    const result = retargetExit(this.graph, this.node.id, this.continueTarget);
    this.graphChange.emit(result.graph);
    this.selectedIdChange.emit(result.selectedId);
    this.close.emit();
  }
  kindLabel(kind: AutomationNode['kind']): string { return kind.replace('_', ' ').replace(/^./, (letter) => letter.toUpperCase()); }

  private updateNode(patch: Partial<AutomationNode>): void {
    if (!this.node || this.readOnly) return;
    this.graphChange.emit({ ...this.graph, nodes: this.graph.nodes.map((node) => node.id === this.node!.id ? { ...node, ...patch } : node) });
  }
  private canReach(from: string, to: string): boolean {
    const seen = new Set([from]);
    const queue = [from];
    while (queue.length) {
      const id = queue.shift()!;
      for (const edge of this.graph.edges.filter((edge) => edge.from === id)) {
        if (edge.to === to) return true;
        if (!seen.has(edge.to)) { seen.add(edge.to); queue.push(edge.to); }
      }
    }
    return false;
  }
}
