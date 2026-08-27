import { Component } from '@angular/core';
import { ComponentFixture, TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { TagChipInput } from './tag-chip-input';

@Component({
  standalone: true,
  imports: [TagChipInput],
  template: `<mf-tag-chip-input [tags]="tags" (tagsChange)="tags = $event" />`,
})
class HostComponent {
  tags = ['customer'];
}

describe('TagChipInput', () => {
  function mount(): ComponentFixture<HostComponent> {
    const fixture = TestBed.createComponent(HostComponent);
    fixture.detectChanges();
    return fixture;
  }

  it('adds trimmed unique tags on Enter', () => {
    const fixture = mount();
    const input = fixture.nativeElement.querySelector(
      '[data-testid="tag-chip-text"]',
    ) as HTMLInputElement;
    input.value = ' vip ';
    input.dispatchEvent(new Event('input'));
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter' }));
    fixture.detectChanges();
    expect(fixture.componentInstance.tags).toEqual(['customer', 'vip']);
  });

  it('removes a chip from the replacement set', () => {
    const fixture = mount();
    (
      fixture.nativeElement.querySelector('[data-testid="tag-chip-remove"]') as HTMLButtonElement
    ).click();
    fixture.detectChanges();
    expect(fixture.componentInstance.tags).toEqual([]);
  });
});
