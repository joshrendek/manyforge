import { HttpErrorResponse } from '@angular/common/http';
import { Component, HostListener, OnInit, computed, inject, signal } from '@angular/core';
import { ActivatedRoute, RouterLink } from '@angular/router';
import { forkJoin, map, Observable, switchMap, throwError } from 'rxjs';
import {
  Automation,
  AutomationGraph,
  AutomationIssue,
  AutomationsService,
  AutomationVersion,
} from '../../../core/automations.service';
import { CurrentBusinessService } from '../../../core/current-business.service';
import { MailingList, MailingService, MailingTemplate } from '../../../core/mailing.service';
import { HasUnsavedChanges, protectBeforeUnload } from '../../../core/unsaved-changes.guard';
import { Spinner } from '../../../ui/spinner/spinner';
import { StatusPill } from '../../../ui/status-pill/status-pill';
import { automationStatusTone } from '../../../ui/status';
import { ToastService } from '../../../ui/toast/toast.service';
import { AutomationCanvasComponent } from './canvas/automation-canvas';
import { deleteNode, GraphReferences, starterGraph, validateGraph } from './canvas/graph-ops';
import { AutomationNodePanelComponent } from './canvas/node-panel';

@Component({
  selector: 'app-automation-editor',
  standalone: true,
  imports: [RouterLink, Spinner, StatusPill, AutomationCanvasComponent, AutomationNodePanelComponent],
  template: `
    <div class="editor" data-testid="automation-editor">
      <header class="header">
        <div class="title">
          <a routerLink="/mailing/automations" class="back" data-testid="automation-back">← Automations</a>
          <div class="title-line"><h1>{{ automation()?.name || 'Automation' }}</h1>@if (automation(); as auto) { <mf-status-pill [tone]="statusTone(auto.status)" [label]="auto.status" [ariaLabel]="'Automation status'" /> }@if (version(); as currentVersion) { <span class="version">Version {{ currentVersion.number }} · {{ currentVersion.status }}</span> }</div>
        </div>
        <div class="actions">
          @if (loading()) { <mf-spinner /> }
          <span class="validation" [class.invalid]="allIssues().length" data-testid="automation-validation-count">{{ allIssues().length ? allIssues().length + ' issue(s)' : 'Graph valid' }}</span>
          <button type="button" class="mf-btn mf-btn-ghost mf-btn-sm" data-testid="automation-discard" [disabled]="!dirty() || saving() || acting()" (click)="discard()">Discard</button>
          <button type="button" class="mf-btn mf-btn-primary mf-btn-sm" data-testid="automation-save" [disabled]="!canSave() || acting()" (click)="save()">{{ saving() ? 'Saving…' : 'Save' }}</button>
          @if (automation(); as auto) {
            @switch (auto.status) {
              @case ('draft') {
                <button type="button" class="mf-btn mf-btn-primary" data-testid="automation-activate" [disabled]="!canActivate()" (click)="activate()">{{ acting() === 'activate' ? 'Activating…' : 'Activate' }}</button>
              }
              @case ('active') {
                <button type="button" class="mf-btn mf-btn-secondary" data-testid="automation-pause" [disabled]="!!acting()" (click)="pause()">{{ acting() === 'pause' ? 'Pausing…' : 'Pause' }}</button>
              }
              @case ('paused') {
                <button type="button" class="mf-btn mf-btn-secondary" data-testid="automation-resume" [disabled]="!!acting()" (click)="resume()">{{ acting() === 'resume' ? 'Resuming…' : 'Resume' }}</button>
              }
            }
            @if ((auto.status === 'active' || auto.status === 'paused') && !auto.draft_version_id) {
              <button type="button" class="mf-btn mf-btn-ghost" data-testid="automation-edit" [disabled]="!!acting()" (click)="edit()">Edit</button>
            }
            @if (auto.status !== 'archived') {
              <button type="button" class="mf-btn mf-btn-danger" data-testid="automation-archive" [disabled]="!!acting()" (click)="archive()">{{ acting() === 'archive' ? 'Archiving…' : 'Archive' }}</button>
            }
          }
        </div>
      </header>
      <nav class="tabs" aria-label="Automation details"><button type="button" class="tab active" data-testid="automation-tab-canvas">Canvas</button><button type="button" class="tab" data-testid="automation-tab-enrollments" disabled title="Enrollment history arrives in the next frontend slice">Enrollments</button></nav>
      @if (editingLive()) {
        <div class="notice" data-testid="automation-version-banner">Editing v{{ version()?.number }} — the active version stays live until you activate this draft.</div>
      }
      @if (error()) { <div class="error" data-testid="automation-editor-error">{{ error() }}</div> }
      @if (!loading() && !lists().length) { <div class="notice" data-testid="automation-no-lists">Create an active mailing list before building this automation.</div> }
      @if (!loading() && version()) {
        <main class="workspace">
          <app-automation-canvas
            [graph]="graph()"
            [selectedId]="selectedId()"
            [issues]="allIssues()"
            [readOnly]="readOnly()"
            [references]="references"
            (graphChange)="setGraph($event)"
            (selectedIdChange)="selectedId.set($event)"
            (openNode)="openPanel($event)"
          />
          @if (panelOpen() && selectedId()) {
            <app-automation-node-panel
              [graph]="graph()"
              [selectedId]="selectedId()"
              [lists]="lists()"
              [templates]="templates()"
              [issues]="allIssues()"
              [readOnly]="readOnly()"
              (graphChange)="setGraph($event)"
              (selectedIdChange)="selectedId.set($event)"
              (close)="closePanel()"
              (deleteRequested)="removeNode($event)"
            />
          }
        </main>
      }
    </div>
  `,
  styles: [`
    :host{display:block}.editor{margin:-28px;min-height:calc(100vh - 56px);display:flex;flex-direction:column;background:var(--mf-surface)}.header{display:flex;justify-content:space-between;align-items:center;gap:20px;padding:14px 20px;border-bottom:1px solid var(--mf-border)}.title{min-width:0}.back{font-size:var(--mf-fs-xs);color:var(--mf-text-muted)}.title-line,.actions{display:flex;align-items:center;gap:10px}.title h1{margin:3px 0 0;font-size:var(--mf-fs-xl);white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.version,.validation{font-size:var(--mf-fs-xs);color:var(--mf-text-muted)}.validation.invalid{color:var(--mf-danger-text)}.tabs{display:flex;padding:0 20px;border-bottom:1px solid var(--mf-border)}.tab{padding:11px 14px;border:0;border-bottom:2px solid transparent;color:var(--mf-text-muted);background:transparent}.tab.active{border-bottom-color:var(--mf-accent);color:var(--mf-text);font-weight:700}.workspace{display:grid;grid-template-columns:minmax(0,1fr) auto;flex:1;min-height:0;height:calc(100vh - 145px)}.error,.notice{padding:10px 20px;font-size:var(--mf-fs-sm)}.error{color:var(--mf-danger-text);background:var(--mf-danger-soft)}.notice{color:var(--mf-warn-text);background:var(--mf-warn-soft)}@media(max-width:760px){.header{align-items:flex-start;flex-direction:column}.workspace{height:700px}.actions{flex-wrap:wrap}:host ::ng-deep app-automation-node-panel .panel{width:290px}}
  `],
})
export class AutomationEditorComponent implements OnInit, HasUnsavedChanges {
  private readonly route = inject(ActivatedRoute);
  private readonly automations = inject(AutomationsService);
  private readonly mailing = inject(MailingService);
  private readonly current = inject(CurrentBusinessService);
  private readonly toast = inject(ToastService);
  private savedJson = signal('');

  businessId = '';
  automationId = '';
  automation = signal<Automation | null>(null);
  version = signal<AutomationVersion | null>(null);
  graph = signal<AutomationGraph>({ nodes: [], edges: [] });
  lists = signal<MailingList[]>([]);
  templates = signal<MailingTemplate[]>([]);
  selectedId = signal<string | null>(null);
  panelOpen = signal(false);
  serverErrors = signal<AutomationIssue[]>([]);
  loading = signal(true);
  saving = signal(false);
  acting = signal('');
  error = signal('');
  readonly clientErrors = computed(() => validateGraph(this.graph()));
  readonly allIssues = computed(() => [...this.clientErrors(), ...this.serverErrors()]);
  readonly readOnly = computed(() => this.version()?.status !== 'draft');
  readonly dirty = computed(() => !!this.savedJson() && JSON.stringify(this.graph()) !== this.savedJson());
  readonly editingLive = computed(() => {
    const version = this.version();
    const automation = this.automation();
    return !!version && version.status === 'draft' && !!automation && automation.draft_version_id === version.id && !!automation.active_version_id;
  });
  readonly canActivate = computed(() => !this.readOnly() && !this.dirty() && this.clientErrors().length === 0 && !this.saving() && this.acting() === '');
  readonly references: GraphReferences = {};
  readonly statusTone = automationStatusTone;

  ngOnInit(): void {
    this.businessId = this.route.snapshot.paramMap.get('businessId') ?? '';
    this.automationId = this.route.snapshot.paramMap.get('automationId') ?? '';
    if (!this.businessId || !this.automationId) {
      this.loading.set(false);
      this.error.set('Automation route is invalid');
      return;
    }
    this.current.set(this.businessId);
    this.load();
  }
  load(): void {
    forkJoin({
      automation: this.automations.get(this.businessId, this.automationId),
      lists: this.mailing.listAllLists(this.businessId),
      templates: this.mailing.listAllTemplates(this.businessId),
    }).pipe(
      switchMap((loaded) => {
        const versionId = loaded.automation.draft_version_id ?? loaded.automation.active_version_id;
        if (!versionId) return throwError(() => new Error('Automation has no version'));
        return this.automations.version(this.businessId, this.automationId, versionId).pipe(
          map((version) => ({ ...loaded, version })),
        );
      }),
    ).subscribe({
      next: ({ automation, lists, templates, version }) => {
        const activeLists = lists.filter((list) => list.status === 'active');
        this.automation.set(automation);
        this.version.set(version);
        this.lists.set(activeLists);
        this.templates.set(templates);
        this.savedJson.set(JSON.stringify(version.graph));
        const graph = version.graph.nodes.length || !activeLists.length ? version.graph : starterGraph(activeLists[0].id);
        this.graph.set(graph);
        this.references.listId = activeLists[0]?.id;
        this.references.templateId = templates[0]?.id;
        this.selectedId.set(graph.nodes[0]?.id ?? null);
        this.loading.set(false);
      },
      error: () => { this.loading.set(false); this.error.set('Could not load automation'); },
    });
  }

  hasUnsavedChanges(): boolean { return !this.readOnly() && this.dirty(); }
  canSave(): boolean { return !this.readOnly() && this.dirty() && !this.saving() && this.clientErrors().length === 0; }
  @HostListener('window:beforeunload', ['$event']) beforeUnload(event: BeforeUnloadEvent): void { protectBeforeUnload(event, this.hasUnsavedChanges()); }

  setGraph(graph: AutomationGraph): void {
    if (this.readOnly()) return;
    this.graph.set(graph);
    this.serverErrors.set([]);
  }
  openPanel(id: string): void {
    if (!id) { this.closePanel(); return; }
    this.selectedId.set(id);
    this.panelOpen.set(true);
  }
  closePanel(): void { this.panelOpen.set(false); }
  discard(): void {
    if (!this.savedJson() || this.saving()) return;
    this.graph.set(JSON.parse(this.savedJson()) as AutomationGraph);
    this.serverErrors.set([]);
    this.selectedId.set(this.graph().nodes[0]?.id ?? null);
    this.panelOpen.set(false);
  }
  removeNode(id: string): void {
    try {
      const result = deleteNode(this.graph(), id);
      if (result.removedIds.length > 1 && !globalThis.confirm(`Delete this condition and ${result.removedIds.length - 1} step(s) used only by its No branch?`)) return;
      this.setGraph(result.graph);
      this.selectedId.set(result.selectedId);
      this.panelOpen.set(false);
    } catch (error) {
      this.toast.error(error instanceof Error ? error.message : 'This step cannot be deleted');
    }
  }
  save(): void {
    const version = this.version();
    if (!version || !this.canSave()) return;
    this.saving.set(true);
    this.automations.putGraph(this.businessId, this.automationId, version.id, this.graph()).subscribe({
      next: (saved) => {
        this.version.set(saved);
        this.graph.set(saved.graph);
        this.savedJson.set(JSON.stringify(saved.graph));
        this.serverErrors.set([]);
        this.saving.set(false);
        this.toast.success('Automation saved');
      },
      error: (response: HttpErrorResponse) => {
        this.saving.set(false);
        const issues = response.error?.issues;
        if (response.status === 422 && Array.isArray(issues)) this.serverErrors.set(issues);
        this.toast.error('Could not save automation');
      },
    });
  }
  activate(): void {
    const version = this.version();
    if (!version || !this.canActivate()) return;
    this.acting.set('activate');
    this.automations.activate(this.businessId, this.automationId, version.id).subscribe({
      next: (automation) => {
        this.automation.set(automation);
        this.version.update((current) => (current && current.id === version.id ? { ...current, status: 'active' } : current));
        this.serverErrors.set([]);
        this.acting.set('');
        this.toast.success('Automation activated');
      },
      error: (response: HttpErrorResponse) => {
        this.acting.set('');
        const issues = response.error?.issues;
        if (response.status === 422 && Array.isArray(issues)) this.serverErrors.set(issues as AutomationIssue[]);
        this.toast.error(response.status === 422 ? 'Fix the highlighted steps before activating' : 'Could not activate automation');
      },
    });
  }
  pause(): void { this.transition('pause', () => this.automations.pause(this.businessId, this.automationId), 'Automation paused'); }
  resume(): void { this.transition('resume', () => this.automations.resume(this.businessId, this.automationId), 'Automation resumed'); }
  edit(): void {
    if (this.acting()) return;
    this.acting.set('edit');
    this.automations.createVersion(this.businessId, this.automationId).subscribe({
      next: (version) => {
        this.acting.set('');
        this.automation.update((automation) => (automation ? { ...automation, draft_version_id: version.id } : automation));
        this.version.set(version);
        this.graph.set(version.graph);
        this.savedJson.set(JSON.stringify(version.graph));
        this.serverErrors.set([]);
        this.selectedId.set(version.graph.nodes[0]?.id ?? null);
        this.panelOpen.set(false);
      },
      error: (response: HttpErrorResponse) => {
        this.acting.set('');
        this.toast.error('Could not create a new draft version');
        if (response.status === 409) this.load();
      },
    });
  }
  archive(): void {
    if (this.acting()) return;
    if (!globalThis.confirm('Archive this automation? Active enrollments will exit and unsaved draft changes will be lost.')) return;
    this.acting.set('archive');
    this.automations.archive(this.businessId, this.automationId).subscribe({
      next: () => {
        this.acting.set('');
        this.toast.success('Automation archived');
        this.load();
      },
      error: (response: HttpErrorResponse) => {
        this.acting.set('');
        this.toast.error('Could not archive automation');
        if (response.status === 409) this.load();
      },
    });
  }
  private transition(action: 'pause' | 'resume', call: () => Observable<Automation>, success: string): void {
    if (this.acting()) return;
    this.acting.set(action);
    call().subscribe({
      next: (automation) => {
        this.automation.set(automation);
        this.acting.set('');
        this.toast.success(success);
      },
      error: (response: HttpErrorResponse) => {
        this.acting.set('');
        this.toast.error('Could not update automation status');
        if (response.status === 409) this.load();
      },
    });
  }
}
