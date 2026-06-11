import { Component } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { StatusPill } from './status-pill';

@Component({ standalone: true, imports: [StatusPill],
  template: `<mf-status-pill tone="danger" label="Urgent" data-testid="p" />` })
class Host {}

describe('mf-status-pill', () => {
  it('renders label and tone class', () => {
    const f = TestBed.createComponent(Host); f.detectChanges();
    const el: HTMLElement = f.nativeElement.querySelector('[data-testid="p"]');
    expect(el.textContent?.trim()).toContain('Urgent');
    expect(el.querySelector('.mf-pill-danger')).toBeTruthy();
  });
});
