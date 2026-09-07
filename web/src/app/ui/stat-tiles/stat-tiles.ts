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
      @for (tile of tiles; track $index) {
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
})
export class StatTiles {
  @Input() tiles: StatTile[] = [];
}
