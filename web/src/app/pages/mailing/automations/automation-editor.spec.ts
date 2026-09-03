import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { ActivatedRoute, convertToParamMap, provideRouter } from '@angular/router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AutomationEditorComponent } from './automation-editor';

const automation = {
  id: 'a1', business_id: 'b1', tenant_root_id: 'b1', name: 'Welcome', description: null,
  status: 'draft', allow_reenroll: false, active_version_id: null, draft_version_id: 'v1',
  created_by_principal_id: 'u1', created_at: '', updated_at: '',
};
const version = {
  id: 'v1', business_id: 'b1', tenant_root_id: 'b1', automation_id: 'a1', number: 1,
  status: 'draft', graph: { nodes: [], edges: [] }, trigger_kind: null, trigger_ref: null,
  activated_at: null, created_at: '', updated_at: '',
};
const list = {
  id: '11111111-1111-4111-8111-111111111111', business_id: 'b1', tenant_root_id: 'b1', slug: 'news', name: 'News',
  description: null, double_opt_in: false, status: 'active', created_at: '', updated_at: '',
};

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

  function mount(): void {
    fixture = TestBed.createComponent(AutomationEditorComponent);
    fixture.detectChanges();
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1').flush(automation);
    http.expectOne('/api/v1/businesses/b1/mailing/lists').flush({ items: [list], next_cursor: null });
    http.expectOne('/api/v1/businesses/b1/mailing/templates').flush({ items: [], next_cursor: null });
    http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/versions/v1').flush(version);
    fixture.detectChanges();
  }

  it('turns an empty draft into an unsaved trigger-to-exit starter', () => {
    mount();
    expect(fixture.componentInstance.graph().nodes.map((node) => node.kind)).toEqual(['trigger', 'exit']);
    expect(fixture.componentInstance.hasUnsavedChanges()).toBe(true);
    expect(fixture.componentInstance.clientErrors()).toEqual([]);
  });

  it('saves the whole graph and resets dirty state', () => {
    mount();
    const graph = fixture.componentInstance.graph();
    fixture.componentInstance.save();
    const request = http.expectOne('/api/v1/businesses/b1/mailing/automations/a1/versions/v1/graph');
    expect(request.request.method).toBe('PUT');
    expect(request.request.body).toEqual(graph);
    request.flush({ ...version, graph });
    expect(fixture.componentInstance.hasUnsavedChanges()).toBe(false);
  });
});
