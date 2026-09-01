import { Component, Input } from '@angular/core';

export interface StatTile {
  label: string;
  value: string | number;
  change?: string;
  detail?: string;
  testid?: string;
}

@Component({
  selector: 'mf-stat-tiles',
  standalone: true,
  template: `
    <div class="mf-stats" data-testid="stat-tiles">
      @for (tile of tiles; track tile.label) {
        <div class="mf-stat" [attr.data-testid]="tile.testid ? tile.testid + '-tile' : null">
          <span class="mf-stat-value" [attr.data-testid]="tile.testid || null">{{
            tile.value
          }}</span>
          <span class="mf-stat-label">{{ tile.label }}</span>
          @if (tile.change) {
            <span
              class="mf-stat-change"
              [attr.data-testid]="tile.testid ? tile.testid + '-change' : null"
              >{{ tile.change }}</span
            >
          }
          @if (tile.detail) {
            <span class="mf-stat-detail">{{ tile.detail }}</span>
          }
        </div>
      }
    </div>
  `,
  styles: [
    `
      .mf-stats {
        display: flex;
        gap: 24px;
        flex-wrap: wrap;
        margin: 16px 0;
      }
      .mf-stat {
        display: flex;
        flex: 1 1 120px;
        flex-direction: column;
        min-width: 120px;
      }
      .mf-stat-value {
        font-size: 28px;
        font-weight: 600;
      }
      .mf-stat-label {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-sm);
      }
      .mf-stat-change,
      .mf-stat-detail {
        color: var(--mf-text-muted);
        font-size: var(--mf-fs-xs);
      }
      .mf-stat-change {
        margin-top: 4px;
      }
    `,
  ],
})
export class StatTiles {
  @Input() tiles: StatTile[] = [];
}
