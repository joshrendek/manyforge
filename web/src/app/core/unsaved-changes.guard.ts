import { CanDeactivateFn } from '@angular/router';

export interface HasUnsavedChanges {
  hasUnsavedChanges(): boolean;
}

export const unsavedChangesGuard: CanDeactivateFn<HasUnsavedChanges> = (component) =>
  !component.hasUnsavedChanges() || globalThis.confirm('Discard your unsaved changes?');

export function protectBeforeUnload(event: BeforeUnloadEvent, dirty: boolean): void {
  if (!dirty) return;
  event.preventDefault();
  event.returnValue = '';
}
