import { TestBed } from '@angular/core/testing';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ToastService } from './toast.service';

describe('ToastService', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('adds a success toast then auto-dismisses', () => {
    const svc = TestBed.inject(ToastService);
    svc.success('Saved');
    expect(svc.toasts().length).toBe(1);
    expect(svc.toasts()[0]).toMatchObject({ kind: 'success', message: 'Saved' });
    vi.advanceTimersByTime(5000);
    expect(svc.toasts().length).toBe(0);
  });

  it('adds an error toast', () => {
    const svc = TestBed.inject(ToastService);
    svc.error('Boom');
    expect(svc.toasts()[0].kind).toBe('error');
  });
});
