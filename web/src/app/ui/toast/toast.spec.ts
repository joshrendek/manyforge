import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { ToastHost } from './toast';
import { ToastService } from './toast.service';

describe('mf-toast-host', () => {
  it('renders queued toasts with testids', () => {
    const f = TestBed.createComponent(ToastHost);
    const svc = TestBed.inject(ToastService);
    svc.success('Hi'); f.detectChanges();
    const el: HTMLElement = f.nativeElement;
    expect(el.querySelector('[data-testid="toast"]')?.textContent).toContain('Hi');
  });
});
