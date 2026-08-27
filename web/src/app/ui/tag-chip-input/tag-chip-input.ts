import { Component, EventEmitter, Input, Output } from '@angular/core';
import { FormsModule } from '@angular/forms';

@Component({
  selector: 'mf-tag-chip-input',
  standalone: true,
  imports: [FormsModule],
  template: `
    <div class="chips" data-testid="tag-chip-input">
      @for (tag of tags; track tag) {
        <span class="mf-pill mf-pill-neutral chip" [attr.data-testid]="chipTestId">
          {{ tag }}
          <button
            type="button"
            class="chip-x"
            [attr.data-testid]="removeTestId"
            [attr.aria-label]="'Remove tag ' + tag"
            [disabled]="disabled"
            (click)="remove(tag)"
          >
            ×
          </button>
        </span>
      }
      <input
        type="text"
        class="mf-input chip-input"
        [attr.data-testid]="inputTestId"
        [placeholder]="placeholder"
        [disabled]="disabled"
        [(ngModel)]="draft"
        (keydown)="onKeydown($event)"
        (blur)="add()"
      />
    </div>
  `,
  styles: [
    `
      .chips {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: 6px;
        min-height: 38px;
        padding: 4px;
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        background: var(--mf-surface);
      }
      .chip {
        display: inline-flex;
        align-items: center;
        gap: 4px;
      }
      .chip-x {
        border: 0;
        padding: 0 2px;
        color: inherit;
        background: transparent;
        cursor: pointer;
      }
      .chip-input {
        flex: 1 1 120px;
        min-width: 100px;
        border: 0;
        padding: 4px 6px;
        box-shadow: none;
      }
    `,
  ],
})
export class TagChipInput {
  @Input() tags: string[] = [];
  @Input() disabled = false;
  @Input() placeholder = 'add tag…';
  @Input() inputTestId = 'tag-chip-text';
  @Input() chipTestId = 'tag-chip';
  @Input() removeTestId = 'tag-chip-remove';
  @Output() tagsChange = new EventEmitter<string[]>();

  draft = '';

  onKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Enter' && event.key !== ',') return;
    event.preventDefault();
    this.add();
  }

  add(): void {
    const tag = this.draft.trim().replace(/,$/, '').trim();
    this.draft = '';
    if (!tag || this.tags.some((value) => value.toLowerCase() === tag.toLowerCase())) return;
    this.tagsChange.emit([...this.tags, tag]);
  }

  remove(tag: string): void {
    this.tagsChange.emit(this.tags.filter((value) => value !== tag));
  }
}
