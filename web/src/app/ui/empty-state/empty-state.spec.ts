import { Component } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { EmptyState } from './empty-state';

@Component({ standalone: true, imports: [EmptyState],
  template: `<mf-empty-state icon="✦" title="No tickets yet" data-testid="e">Nothing here.</mf-empty-state>` })
class Host {}

describe('mf-empty-state', () => {
  it('renders icon, title and projected body', () => {
    const f = TestBed.createComponent(Host); f.detectChanges();
    const el: HTMLElement = f.nativeElement.querySelector('[data-testid="e"]');
    expect(el.textContent).toContain('No tickets yet');
    expect(el.textContent).toContain('Nothing here.');
  });
});
