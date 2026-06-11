import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { CurrentBusinessService } from './current-business.service';

describe('CurrentBusinessService', () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => localStorage.clear());

  it('defaults to null and persists a set value', () => {
    const svc = TestBed.inject(CurrentBusinessService);
    expect(svc.businessId()).toBeNull();
    svc.set('b1');
    expect(svc.businessId()).toBe('b1');
    expect(localStorage.getItem('mf-current-business')).toBe('b1');
  });

  it('rehydrates from localStorage', () => {
    localStorage.setItem('mf-current-business', 'b2');
    expect(TestBed.inject(CurrentBusinessService).businessId()).toBe('b2');
  });
});
