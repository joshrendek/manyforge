import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { Spinner } from './spinner';

describe('mf-spinner', () => {
  it('renders an aria-busy element', () => {
    const f = TestBed.createComponent(Spinner); f.detectChanges();
    expect(f.nativeElement.querySelector('[aria-busy="true"]')).toBeTruthy();
  });
});
