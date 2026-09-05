import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Automation, AutomationGraph, AutomationVersion, AutomationVersionStatus } from '../../../core/automations.service';
import { AutomationEditorComponent } from './automation-editor';

const list = {
  id: '11111111-1111-4111-8111-111111111111', business_id: 'b1', tenant_root_id: 'b1', slug: 'news', name: 'News',
  description: null, double_opt_in: false, status: 'active', created_at: '', updated_at: '',
};
const template = {
  id: '22222222-2222-4222-8222-222222222222', business_id: 'b1', tenant_root_id: 'b1', name: 'Welcome email',
  subject: 'Welcome', preheader: null, body_markdown: 'Hello', track_opens: true, track_clicks: true,
  created_at: '', updated_at: '',
};
const draftAutomation: Automation = {
  id: 'a1', business_id: 'b1', tenant_root_id: 'b1', name: 'Welcome', description: null,
  status: 'draft', allow_reenroll: false, active_version_id: null, draft_version_id: 'v1',
  created_by_principal_id: 'u1', created_at: '', updated_at: '',
};
const activeAutomation: Automation = {
  ...draftAutomation, status: 'active', active_version_id: 'v2', draft_version_id: null,
};

const graph: AutomationGraph = {
  nodes: [
    { id: 'trigger', kind: 'trigger', name: 'Trigger', config: { type: 'list_joined', list_id: list.id } },
    { id: 'n_welcome', kind: 'send_email', name: 'Welcome', config: { template_id: template.id, track_opens: true, track_clicks: true } },
    { id: 'exit', kind: 'exit', config: {} },
  ],
  edges: [
    { id: 'e1', from: 'trigger', to: 'n_welcome', branch: null },
    { id: 'e2', from: 'n_welcome', to: 'exit', branch: null },
  ],
};

function makeVersion(id: string, number: number, status: AutomationVersionStatus, versionGraph: AutomationGraph = JSON.parse(JSON.stringify(graph))): AutomationVersion {
  return {
    id, business_id: 'b1', tenant_root_id: 'b1', automation_id: 'a1', number, status,
    graph: versionGraph, trigger_kind: null, trigger_ref: null,
    activated_at: null, created_at: '', updated_at: '',
  };
}

describe('AutomationEditorComponent', () => {
  let http: HttpTestingController;
  let fixture: ComponentFixture<AutomationEditorComponent>;

  beforeEach(() => {
    localStorage.clear();
    vi.stubGlobal('crypto', { randomUUID: vi.fn().mockReturnValueOnce('trigger').mockReturnValueOnce('exit').mockReturnValueOnce('edge') });
    TestBed.configureTestingModule({
      providers: [
        provideHttpClient(), provideHttpClientTesting(), provideRouter([]),
        { provide: ActivatedRoute, useValue: { snapshot: { paramMap: convertToParamMap({ businessId: 'b1', automationId: 'a1' }) } } },
      ],
    });
    http = TestBed.inject(HttpTestingController);
  });
  afterEach(() => { fixture?.destroy(); http.verify(); localStorage.clear(); vi.unstubAllGlobals(); });

  function mount(overrides: { automation?: Automation; version?: AutomationVersion } = {}): AutomationEditorComponent {
    const automation = overrides.automation ?? draftAutomation;
    const version = overrides.version ?? makeVersion('v1', 1, 'draft', { nodes: [], edges: [] });
    fixture = TestBed.createComponent(AutomationEditorComponent);
    fixture.detectChanges();
    flushReload(automation, version);
    fixture.detectChanges();
    return fixture.componentInstance;
  }

  function flushReload(automation: Automation, version: AutomationVersion): void {
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1').flush(automation);
    http.expectOne('/api/v1/businesses/b1/mailing/lists').flush({ items: [list], next_cursor: null });
    http.expectOne('/api/v1/businesses/b1/mailing/templates').flush({ items: [template], next_cursor: null });
    http.expectOne(`/api/v1/businesses/b1/mailing/automations/a1/versions/${version.id}`).flush(version);
  }

  it('turns an empty draft into an unsaved trigger-to-exit starter', () => {
    const component = mount();
    expect(component.graph().nodes.map((node) => node.kind)).toEqual(['trigger', 'exit']);
    expect(component.hasUnsavedChanges()).toBe(true);
    expect(component.clientErrors()).toEqual([]);
    expect(fixture.nativeElement.querySelector('[data-testid="automation-version-banner"]')).toBeNull();
  });

  it('saves the whole graph and resets dirty state', () => {
    const component = mount();
    const graphToSave = component.graph();
    component.save();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/versions/v1/graph');
    expect(request.request.method).toBe('PUT');
    expect(request.request.body).toEqual(graphToSave);
    request.flush({ ...makeVersion('v1', 1, 'draft'), graph: graphToSave });
    expect(component.hasUnsavedChanges()).toBe(false);
  });

  it('mirrors server activation errors onto the canvas by node_id', () => {
    const component = mount({ version: makeVersion('v1', 1, 'draft') });
    const activate = fixture.nativeElement.querySelector('[data-testid="automation-activate"]') as HTMLButtonElement;
    expect(activate.disabled).toBe(false);
    activate.click();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/versions/v1/activate');
    expect(request.request.method).toBe('POST');
    request.flush(
      { code: 'AUTOMATION_INVALID', message: 'automation graph is invalid', issues: [{ code: 'template_not_found', node_id: 'n_welcome', message: 'Template not found' }] },
      { status: 422, statusText: 'Unprocessable Entity' },
    );
    fixture.detectChanges();
    expect(component.serverErrors()).toHaveLength(1);
    expect(fixture.nativeElement.querySelector('[data-node-id="n_welcome"]').getAttribute('data-invalid')).toBe('true');
    expect(fixture.nativeElement.querySelector('[data-testid="automation-validation-count"]').textContent).toContain('1 issue');
  });

  it('flips the version to active and read-only on successful activation', () => {
    const component = mount({ version: makeVersion('v1', 1, 'draft') });
    (fixture.nativeElement.querySelector('[data-testid="automation-activate"]') as HTMLButtonElement).click();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/versions/v1/activate');
    request.flush({ ...draftAutomation, status: 'active', active_version_id: 'v1', draft_version_id: null });
    fixture.detectChanges();
    expect(component.automation()!.status).toBe('active');
    expect(component.version()!.status).toBe('active');
    expect(component.readOnly()).toBe(true);
    expect(fixture.nativeElement.querySelector('[data-testid="automation-pause"]')).toBeTruthy();
    expect(fixture.nativeElement.querySelector('[data-testid="automation-activate"]')).toBeNull();
    expect(fixture.nativeElement.querySelectorAll('[data-testid="edge-plus"]').length).toBe(0);
  });

  it('disables activate while the graph has unsaved changes', () => {
    const component = mount({ version: makeVersion('v1', 1, 'draft') });
    const current = component.graph();
    component.setGraph({ ...current, nodes: current.nodes.map((node) => (node.id === 'n_welcome' ? { ...node, name: 'Renamed' } : node)) });
    fixture.detectChanges();
    expect(component.canActivate()).toBe(false);
    expect((fixture.nativeElement.querySelector('[data-testid="automation-activate"]') as HTMLButtonElement).disabled).toBe(true);
  });

  it('disables activate while client validation reports issues', () => {
    const component = mount({ version: makeVersion('v1', 1, 'draft') });
    const [trigger] = component.graph().nodes;
    component.setGraph({ nodes: [trigger, { ...trigger, id: 'trigger_2' }], edges: [] });
    expect(component.clientErrors().length).toBeGreaterThan(0);
    expect(component.canActivate()).toBe(false);
  });

  it('creates a draft from a live automation and shows the version banner', () => {
    const component = mount({ automation: activeAutomation, version: makeVersion('v2', 2, 'active') });
    expect(fixture.nativeElement.querySelector('[data-testid="automation-version-banner"]')).toBeNull();
    (fixture.nativeElement.querySelector('[data-testid="automation-edit"]') as HTMLButtonElement).click();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/versions');
    expect(request.request.method).toBe('POST');
    request.flush(makeVersion('v3', 3, 'draft'), { status: 201, statusText: 'Created' });
    fixture.detectChanges();
    expect(component.version()!.id).toBe('v3');
    expect(component.editingAlongsideLive()).toBe(true);
    expect(component.readOnly()).toBe(false);
    const banner = fixture.nativeElement.querySelector('[data-testid="automation-version-banner"]');
    expect(banner.textContent).toContain('stays live');
    expect(fixture.nativeElement.querySelectorAll('[data-testid="edge-plus"]').length).toBeGreaterThan(0);
  });

  it('shows the version banner when loading a live automation that already has a draft', () => {
    mount({
      automation: { ...draftAutomation, status: 'active', active_version_id: 'v2', draft_version_id: 'v3' },
      version: makeVersion('v3', 3, 'draft'),
    });
    const banner = fixture.nativeElement.querySelector('[data-testid="automation-version-banner"]');
    expect(banner).toBeTruthy();
    expect(banner.textContent).toContain('stays live');
  });

  it('pauses and resumes an active automation', () => {
    const component = mount({ automation: activeAutomation, version: makeVersion('v2', 2, 'active') });
    (fixture.nativeElement.querySelector('[data-testid="automation-pause"]') as HTMLButtonElement).click();
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/pause')
      .flush({ ...activeAutomation, status: 'paused' });
    fixture.detectChanges();
    expect(component.automation()!.status).toBe('paused');
    expect(fixture.nativeElement.querySelector('[data-testid="automation-resume"]')).toBeTruthy();
    (fixture.nativeElement.querySelector('[data-testid="automation-resume"]') as HTMLButtonElement).click();
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/resume')
      .flush({ ...activeAutomation, status: 'active' });
    fixture.detectChanges();
    expect(component.automation()!.status).toBe('active');
  });

  it('archives after confirmation and reloads the read-only state', () => {
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true));
    const component = mount({ automation: activeAutomation, version: makeVersion('v2', 2, 'active') });
    (fixture.nativeElement.querySelector('[data-testid="automation-archive"]') as HTMLButtonElement).click();
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/archive')
      .flush({ ...activeAutomation, status: 'archived' });
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1')
      .flush({ ...activeAutomation, status: 'archived' });
    http.expectOne('/api/v1/businesses/b1/mailing/lists').flush({ items: [list], next_cursor: null });
    http.expectOne('/api/v1/businesses/b1/mailing/templates').flush({ items: [template], next_cursor: null });
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/versions/v2').flush(makeVersion('v2', 2, 'active'));
    fixture.detectChanges();
    expect(component.automation()!.status).toBe('archived');
    expect(component.readOnly()).toBe(true);
    expect(fixture.nativeElement.querySelector('[data-testid="automation-archive"]')).toBeNull();
  });

  it('does not archive when confirmation is declined', () => {
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(false));
    mount({ automation: activeAutomation, version: makeVersion('v2', 2, 'active') });
    (fixture.nativeElement.querySelector('[data-testid="automation-archive"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    http.expectNone('/api/v1/businesses/b1/mailing/automations/a1/archive');
  });

  it('reloads when activation hits a lifecycle conflict', () => {
    const component = mount({ version: makeVersion('v1', 1, 'draft') });
    (fixture.nativeElement.querySelector('[data-testid="automation-activate"]') as HTMLButtonElement).click();
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/versions/v1/activate')
      .flush({ code: 'CONFLICT', message: 'lifecycle conflict' }, { status: 409, statusText: 'Conflict' });
    flushReload(draftAutomation, makeVersion('v1', 1, 'draft'));
    fixture.detectChanges();
    expect(component.automation()!.status).toBe('draft');
    expect(component.version()!.status).toBe('draft');
    expect(component.serverErrors()).toEqual([]);
  });

  it('reloads when pausing hits a lifecycle conflict', () => {
    const component = mount({ automation: activeAutomation, version: makeVersion('v2', 2, 'active') });
    (fixture.nativeElement.querySelector('[data-testid="automation-pause"]') as HTMLButtonElement).click();
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/pause')
      .flush({ code: 'CONFLICT', message: 'lifecycle conflict' }, { status: 409, statusText: 'Conflict' });
    flushReload(activeAutomation, makeVersion('v2', 2, 'active'));
    fixture.detectChanges();
    expect(component.automation()!.status).toBe('active');
  });

  it('reloads when creating a draft hits a conflict', () => {
    const component = mount({ automation: activeAutomation, version: makeVersion('v2', 2, 'active') });
    (fixture.nativeElement.querySelector('[data-testid="automation-edit"]') as HTMLButtonElement).click();
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/versions')
      .flush({ code: 'CONFLICT', message: 'lifecycle conflict' }, { status: 409, statusText: 'Conflict' });
    flushReload(activeAutomation, makeVersion('v2', 2, 'active'));
    fixture.detectChanges();
    expect(component.version()!.id).toBe('v2');
    expect(component.readOnly()).toBe(true);
  });

  it('reloads when archiving hits a conflict', () => {
    vi.stubGlobal('confirm', vi.fn().mockReturnValue(true));
    const component = mount({ automation: activeAutomation, version: makeVersion('v2', 2, 'active') });
    (fixture.nativeElement.querySelector('[data-testid="automation-archive"]') as HTMLButtonElement).click();
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/archive')
      .flush({ code: 'CONFLICT', message: 'lifecycle conflict' }, { status: 409, statusText: 'Conflict' });
    flushReload(activeAutomation, makeVersion('v2', 2, 'active'));
    fixture.detectChanges();
    expect(component.automation()!.status).toBe('active');
  });

  it('discards unsaved changes and clears server errors', () => {
    const component = mount({ version: makeVersion('v1', 1, 'draft') });
    const saved = JSON.stringify(component.graph());
    const current = component.graph();
    component.setGraph({ ...current, nodes: current.nodes.map((node) => (node.id === 'n_welcome' ? { ...node, name: 'Renamed' } : node)) });
    component.serverErrors.set([{ code: 'template_not_found', node_id: 'n_welcome', message: 'Template not found' }]);
    expect(component.hasUnsavedChanges()).toBe(true);
    fixture.detectChanges();
    (fixture.nativeElement.querySelector('[data-testid="automation-discard"]') as HTMLButtonElement).click();
    fixture.detectChanges();
    expect(JSON.stringify(component.graph())).toBe(saved);
    expect(component.hasUnsavedChanges()).toBe(false);
    expect(component.serverErrors()).toEqual([]);
  });
});
