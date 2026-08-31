import { describe, expect, it, vi } from 'vitest';
import {
  HasUnsavedChanges,
  protectBeforeUnload,
  unsavedChangesGuard,
} from './unsaved-changes.guard';

describe('unsavedChangesGuard', () => {
  it('allows clean navigation without prompting', () => {
    const confirm = vi.spyOn(globalThis, 'confirm');
    const result = unsavedChangesGuard(
      { hasUnsavedChanges: () => false },
      {} as never,
      {} as never,
      {} as never,
    );
    expect(result).toBe(true);
    expect(confirm).not.toHaveBeenCalled();
    confirm.mockRestore();
  });

  it('uses the browser confirmation result for dirty components', () => {
    const component: HasUnsavedChanges = { hasUnsavedChanges: () => true };
    const confirm = vi.spyOn(globalThis, 'confirm').mockReturnValue(false);
    expect(unsavedChangesGuard(component, {} as never, {} as never, {} as never)).toBe(false);
    confirm.mockReturnValue(true);
    expect(unsavedChangesGuard(component, {} as never, {} as never, {} as never)).toBe(true);
    confirm.mockRestore();
  });

  it('marks beforeunload only when content is dirty', () => {
    const clean = new Event('beforeunload') as BeforeUnloadEvent;
    const dirty = new Event('beforeunload', { cancelable: true }) as BeforeUnloadEvent;
    protectBeforeUnload(clean, false);
    protectBeforeUnload(dirty, true);
    expect(clean.defaultPrevented).toBe(false);
    expect(dirty.defaultPrevented).toBe(true);
  });
});
