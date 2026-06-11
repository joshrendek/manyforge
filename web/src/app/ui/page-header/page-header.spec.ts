import { Component } from '@angular/core';
import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { PageHeader } from './page-header';

@Component({ standalone: true, imports: [PageHeader],
  template: `<mf-page-header title="Support" subtitle="12 open"><button actions data-testid="a">New</button></mf-page-header>` })
class Host {}

describe('mf-page-header', () => {
  it('renders title, subtitle and projected actions', () => {
    const f = TestBed.createComponent(Host); f.detectChanges();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('h1')?.textContent).toContain('Support');
    expect(el.textContent).toContain('12 open');
    expect(el.querySelector('[data-testid="a"]')).toBeTruthy();
  });
});
