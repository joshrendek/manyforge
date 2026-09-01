import { ComponentFixture, TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { StatTiles } from './stat-tiles';

describe('StatTiles', () => {
  it('renders values, labels, comparisons, and detail text', () => {
    const fixture: ComponentFixture<StatTiles> = TestBed.createComponent(StatTiles);
    fixture.componentInstance.tiles = [
      {
        label: 'Delivered',
        value: '1,234',
        change: '+12% from previous',
        detail: '98.7% of recipients',
        testid: 'stat-delivered',
      },
    ];
    fixture.detectChanges();

    expect(fixture.nativeElement.querySelector('[data-testid="stat-delivered"]').textContent).toBe(
      '1,234',
    );
    expect(fixture.nativeElement.textContent).toContain('Delivered');
    expect(fixture.nativeElement.textContent).toContain('+12% from previous');
    expect(fixture.nativeElement.textContent).toContain('98.7% of recipients');
  });

  it('keeps distinct values when labels are duplicated and tiles update', () => {
    const fixture: ComponentFixture<StatTiles> = TestBed.createComponent(StatTiles);
    fixture.componentRef.setInput('tiles', [
      { label: 'Total', value: '10', testid: 'first-total' },
      { label: 'Total', value: '20', testid: 'second-total' },
    ]);
    fixture.detectChanges();
    fixture.componentRef.setInput('tiles', [
      { label: 'Total', value: '11', testid: 'first-total' },
      { label: 'Total', value: '21', testid: 'second-total' },
    ]);
    fixture.detectChanges();

    expect(fixture.nativeElement.querySelector('[data-testid="first-total"]').textContent).toBe(
      '11',
    );
    expect(fixture.nativeElement.querySelector('[data-testid="second-total"]').textContent).toBe(
      '21',
    );
  });
});
