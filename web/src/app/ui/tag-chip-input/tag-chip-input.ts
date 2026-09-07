import { Component, EventEmitter, Input, Output } from '@angular/core';
import { FormsModule } from '@angular/forms';

@Component({
  selector: 'mf-tag-chip-input',
  standalone: true,
  imports: [FormsModule],
  template: `
    <div class="mf-chips" data-testid="tag-chip-input">
      @for (tag of tags; track tag) {
        <span class="mf-pill mf-pill-neutral" [attr.data-testid]="chipTestId">
          {{ tag }}
          <button
            type="button"
            class="mf-chip-x"
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
        class="mf-input mf-chip-input"
        [attr.data-testid]="inputTestId"
        [placeholder]="placeholder"
        [disabled]="disabled"
        [(ngModel)]="draft"
        (keydown)="onKeydown($event)"
        (blur)="add()"
      />
    </div>
  `,
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
