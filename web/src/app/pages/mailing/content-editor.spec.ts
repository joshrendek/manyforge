import { TestBed } from '@angular/core/testing';
import { describe, expect, it, vi } from 'vitest';
import { MailingContentEditorComponent } from './content-editor';

const content = {
  subject: 'Hello',
  preheader: '',
  body_markdown: 'Hello world',
  track_opens: true,
  track_clicks: true,
};

describe('MailingContentEditorComponent', () => {
  it('inserts variables at the selected body range without mutating its input', async () => {
    const fixture = TestBed.createComponent(MailingContentEditorComponent);
    fixture.componentRef.setInput('value', content);
    fixture.detectChanges();
    await fixture.whenStable();
    fixture.detectChanges();
    const emitted = vi.fn();
    fixture.componentInstance.valueChange.subscribe(emitted);
    const textarea = fixture.nativeElement.querySelector('textarea') as HTMLTextAreaElement;
    textarea.setSelectionRange(6, 11);

    fixture.componentInstance.insertVariable('first_name');
    await Promise.resolve();

    expect(emitted).toHaveBeenCalledWith({
      ...content,
      body_markdown: 'Hello {{first_name}}',
    });
    expect(content.body_markdown).toBe('Hello world');
    expect(textarea.selectionStart).toBe('Hello {{first_name}}'.length);
  });

  it('does not emit edits when read-only', () => {
    const fixture = TestBed.createComponent(MailingContentEditorComponent);
    fixture.componentRef.setInput('value', content);
    fixture.componentRef.setInput('readOnly', true);
    const emitted = vi.fn();
    fixture.componentInstance.valueChange.subscribe(emitted);
    fixture.detectChanges();
    fixture.componentInstance.change({ subject: 'Changed' });
    expect(emitted).not.toHaveBeenCalled();
  });
});
