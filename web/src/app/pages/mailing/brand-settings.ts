import { HttpErrorResponse } from '@angular/common/http';
import {
  Component,
  DestroyRef,
  OnInit,
  computed,
  effect,
  inject,
  signal,
  untracked,
} from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { FormsModule } from '@angular/forms';
import { RouterLink } from '@angular/router';
import { Subject, catchError, forkJoin, of, takeUntil, throwError } from 'rxjs';
import { BusinessService } from '../../core/business.service';
import { CurrentBusinessService } from '../../core/current-business.service';
import {
  MailingBrand,
  MailingBrandColors,
  MailingBrandInput,
  MailingSendingProfile,
  MailingService,
  MailingSetup,
} from '../../core/mailing.service';
import { Business } from '../../core/tree';
import { PageHeader } from '../../ui/page-header/page-header';
import { Spinner } from '../../ui/spinner/spinner';
import { ToastService } from '../../ui/toast/toast.service';
import { MailingContentDraft } from './content-editor';
import { MailingPreviewPaneComponent } from './preview-pane';

/** Server defaults for a brand without explicit colors (mirrors mailrender.DefaultColors). */
export const MAILING_BRAND_DEFAULT_COLORS: MailingBrandColors = {
  background: '#f4f6f8',
  surface: '#ffffff',
  text: '#17212b',
  accent: '#1769aa',
  header_background: '#ffffff',
  header_text: '#17212b',
};

export const MAILING_BRAND_LOGO_MAX_BYTES = 512 * 1024;

const COLOR_FIELDS: ReadonlyArray<{ key: keyof MailingBrandColors; label: string }> = [
  { key: 'background', label: 'Page background' },
  { key: 'surface', label: 'Card surface' },
  { key: 'text', label: 'Body text' },
  { key: 'accent', label: 'Links and buttons' },
  { key: 'header_background', label: 'Header background' },
  { key: 'header_text', label: 'Header text' },
];

const HEX_COLOR = /^#[0-9a-f]{6}$/;
const LOOSE_HEX_COLOR = /^#?([0-9a-f]{6})$/i;

const SAMPLE_CONTENT: MailingContentDraft = {
  subject: 'Brand preview',
  preheader: 'How your emails will look',
  body_markdown: [
    '# Welcome aboard',
    '',
    'This sample message shows your logo, colors, and footer exactly as subscribers will see them.',
    '',
    '[Visit our site](https://example.com)',
    '',
    '- Product updates',
    '- Release notes',
    '',
    'Thanks for reading!',
  ].join('\n'),
  track_opens: false,
  track_clicks: false,
};

@Component({
  selector: 'app-mailing-brand-settings',
  standalone: true,
  imports: [FormsModule, RouterLink, PageHeader, Spinner, MailingPreviewPaneComponent],
  template: `
    <div class="mf-card page" data-testid="mailing-brand-page">
      <mf-page-header
        [eyebrow]="businessName()"
        title="Brand"
        subtitle="Logo, colors, and footer applied to every campaign, template, and confirmation email."
      >
        <a
          routerLink="/mailing/campaigns"
          class="mf-btn mf-btn-ghost mf-btn-sm"
          data-testid="brand-campaigns-link"
          actions
          >Campaigns</a
        >
        <a
          routerLink="/mailing/sending"
          class="mf-btn mf-btn-ghost mf-btn-sm"
          data-testid="brand-sending-link"
          actions
          >Sending profile</a
        >
      </mf-page-header>
      @if (loading()) {
        <p class="loading" data-testid="brand-loading"><mf-spinner /> Loading brand…</p>
      }
      @if (error()) {
        <p class="mf-err" role="alert" data-testid="brand-error">{{ error() }}</p>
      }
      @if (businessId() && !loading() && !error()) {
        <div class="workspace">
          <form class="form-pane" data-testid="brand-form" (ngSubmit)="save()">
            <div class="mf-field">
              <label for="brand-name">Brand name</label>
              <input
                id="brand-name"
                class="mf-input"
                name="name"
                data-testid="brand-name"
                maxlength="200"
                required
                [ngModel]="form().name"
                (ngModelChange)="patch({ name: $event })"
              />
              <span class="mf-hint">
                Shown in the email header when there is no logo, and used as the logo's alt text.
              </span>
            </div>

            <fieldset class="colors">
              <legend>Colors</legend>
              <div class="color-grid">
                @for (field of colorFields; track field.key) {
                  <div class="mf-field" [class.mf-invalid]="!colorValid(field.key)">
                    <label [for]="'brand-color-hex-' + field.key">{{ field.label }}</label>
                    <div class="color-row">
                      <input
                        type="color"
                        [id]="'brand-color-' + field.key"
                        [attr.data-testid]="'brand-color-' + field.key"
                        [attr.aria-label]="field.label + ' picker'"
                        [value]="colorValid(field.key) ? form().colors[field.key] : '#000000'"
                        (input)="pickColor(field.key, $event)"
                      />
                      <input
                        class="mf-input mf-mono"
                        [id]="'brand-color-hex-' + field.key"
                        [name]="'color_' + field.key"
                        [attr.data-testid]="'brand-color-hex-' + field.key"
                        maxlength="7"
                        spellcheck="false"
                        [ngModel]="form().colors[field.key]"
                        (ngModelChange)="patchColor(field.key, $event)"
                      />
                    </div>
                    @if (!colorValid(field.key)) {
                      <span class="mf-err" [attr.data-testid]="'brand-color-error-' + field.key">
                        Use a six-digit hex color such as #1769aa.
                      </span>
                    }
                  </div>
                }
              </div>
            </fieldset>

            <div class="mf-field">
              <label for="brand-font">Font</label>
              <select
                id="brand-font"
                class="mf-select"
                name="fontStack"
                data-testid="brand-font"
                [ngModel]="form().font_stack"
                (ngModelChange)="patch({ font_stack: $event })"
              >
                <option value="system">System sans-serif</option>
                <option value="serif">Serif</option>
                <option value="mono">Monospace</option>
              </select>
            </div>

            <div class="mf-field">
              <label for="brand-footer">Footer <span>(Markdown, optional)</span></label>
              <textarea
                id="brand-footer"
                class="mf-textarea"
                name="footerMarkdown"
                rows="4"
                maxlength="4096"
                data-testid="brand-footer"
                [ngModel]="form().footer_markdown"
                (ngModelChange)="patch({ footer_markdown: $event })"
              ></textarea>
              <span class="mf-hint">
                Rendered above the postal address and unsubscribe link in every email.
              </span>
            </div>

            <section class="logo-card" data-testid="brand-logo-section">
              <h2>Logo</h2>
              @if (brand()?.logo; as logo) {
                <div class="logo-row">
                  <img
                    class="logo-image"
                    data-testid="brand-logo-image"
                    [src]="logo.url"
                    [alt]="form().name || 'Brand logo'"
                    [width]="form().logo_width ?? brand()!.logo_width"
                  />
                  <button
                    type="button"
                    class="mf-btn mf-btn-ghost mf-btn-sm"
                    data-testid="brand-logo-remove"
                    [disabled]="busy()"
                    (click)="removeLogo()"
                  >
                    {{ removingLogo() ? 'Removing…' : 'Remove logo' }}
                  </button>
                </div>
                <div class="mf-field logo-width" [class.mf-invalid]="!logoWidthValid()">
                  <label for="brand-logo-width">Logo width <span>(40–600 px)</span></label>
                  <input
                    id="brand-logo-width"
                    class="mf-input"
                    type="number"
                    name="logoWidth"
                    min="40"
                    max="600"
                    data-testid="brand-logo-width"
                    [ngModel]="form().logo_width"
                    (ngModelChange)="patchLogoWidth($event)"
                  />
                </div>
              }
              @if (logoUploadBlocker(); as blocker) {
                <p class="mf-hint" data-testid="brand-logo-blocker">{{ blocker }}</p>
              }
              <div class="mf-field">
                <label for="brand-logo-file">{{ brand()?.logo ? 'Replace logo' : 'Upload logo' }}</label>
                <input
                  id="brand-logo-file"
                  type="file"
                  name="logo"
                  accept="image/png,image/jpeg,image/gif,image/webp"
                  data-testid="brand-logo-file"
                  [disabled]="!!logoUploadBlocker() || busy()"
                  (change)="onLogoSelected($event)"
                />
                <span class="mf-hint">
                  PNG, JPEG, GIF, or WebP up to 512 KB and 2000×2000 px.
                  {{ uploadingLogo() ? 'Uploading…' : '' }}
                </span>
              </div>
            </section>

            <div class="actions">
              <button
                type="submit"
                class="mf-btn mf-btn-primary"
                data-testid="brand-save"
                [disabled]="!canSave()"
              >
                {{ saving() ? 'Saving…' : brand() ? 'Save brand' : 'Create brand' }}
              </button>
              @if (brand()) {
                @if (!confirmDelete()) {
                  <button
                    type="button"
                    class="mf-btn mf-btn-danger"
                    data-testid="brand-delete"
                    [disabled]="busy()"
                    (click)="confirmDelete.set(true)"
                  >
                    Delete brand
                  </button>
                } @else {
                  <button
                    type="button"
                    class="mf-btn mf-btn-danger"
                    data-testid="brand-delete-confirm"
                    [disabled]="busy()"
                    (click)="remove()"
                  >
                    {{ deleting() ? 'Deleting…' : 'Confirm delete' }}
                  </button>
                  <button
                    type="button"
                    class="mf-btn mf-btn-ghost"
                    data-testid="brand-delete-cancel"
                    [disabled]="busy()"
                    (click)="confirmDelete.set(false)"
                  >
                    Cancel
                  </button>
                }
              }
              @if (brand() && !dirty()) {
                <span class="mf-hint" data-testid="brand-saved-hint">All changes saved.</span>
              }
            </div>
          </form>

          <app-mailing-preview-pane
            [businessId]="businessId()"
            kind="templates"
            [content]="sampleContent"
            [brand]="previewBrand()"
            [fromName]="profile()?.from_name ?? null"
            [postalAddress]="profile()?.postal_address ?? null"
          />
        </div>
      }
    </div>
  `,
  styles: [
    `
      .page {
        display: grid;
        gap: 18px;
      }
      .loading,
      .actions,
      .color-row,
      .logo-row {
        display: flex;
        align-items: center;
        gap: 12px;
      }
      .workspace {
        display: grid;
        grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
        gap: 24px;
      }
      .form-pane {
        display: grid;
        align-content: start;
        gap: 16px;
        min-width: 0;
      }
      .colors {
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius);
        padding: 14px;
        margin: 0;
        min-width: 0;
      }
      .colors legend,
      h2 {
        font-weight: 660;
      }
      .color-grid {
        display: grid;
        grid-template-columns: repeat(2, minmax(0, 1fr));
        gap: 14px;
      }
      .color-row input[type='color'] {
        flex: 0 0 var(--mf-control-h);
        width: var(--mf-control-h);
        height: var(--mf-control-h);
        padding: 2px;
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        background: var(--mf-surface-inset);
        cursor: pointer;
      }
      .color-row .mf-input {
        flex: 1;
      }
      .logo-card {
        display: grid;
        gap: 12px;
        padding: 18px;
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius);
      }
      .logo-card h2 {
        margin: 0;
      }
      .logo-row {
        flex-wrap: wrap;
      }
      .logo-image {
        display: block;
        max-width: 100%;
        height: auto;
        padding: 8px;
        border: 1px dashed var(--mf-border);
        border-radius: var(--mf-radius-sm);
        background: #ffffff;
      }
      .logo-width {
        max-width: 200px;
      }
      label span {
        color: var(--mf-text-muted);
        font-weight: 400;
      }
      .actions {
        flex-wrap: wrap;
        padding-top: 16px;
        border-top: 1px solid var(--mf-border);
      }
      @media (max-width: 960px) {
        .workspace,
        .color-grid {
          grid-template-columns: 1fr;
        }
      }
    `,
  ],
})
export class MailingBrandSettingsComponent implements OnInit {
  private readonly destroyRef = inject(DestroyRef);
  private readonly businessChanged = new Subject<void>();
  private readonly followBusiness = effect(() => {
    const id = this.current.businessId() ?? '';
    untracked(() => {
      if (id !== this.businessId()) this.selectBusiness(id);
    });
  });
  businessName(): string {
    return this.businesses().find((business) => business.id === this.businessId())?.name ?? '';
  }

  private businessesApi = inject(BusinessService);
  private mailing = inject(MailingService);
  private current = inject(CurrentBusinessService);
  private toast = inject(ToastService);

  readonly colorFields = COLOR_FIELDS;
  readonly sampleContent = SAMPLE_CONTENT;

  businesses = signal<Business[]>([]);
  businessId = signal('');
  setup = signal<MailingSetup | null>(null);
  profile = signal<MailingSendingProfile | null>(null);
  brand = signal<MailingBrand | null>(null);
  form = signal<MailingBrandInput>(emptyForm(''));
  private saved = signal<MailingBrandInput | null>(null);
  loading = signal(false);
  saving = signal(false);
  deleting = signal(false);
  uploadingLogo = signal(false);
  removingLogo = signal(false);
  confirmDelete = signal(false);
  error = signal('');

  readonly dirty = computed(() => JSON.stringify(this.form()) !== JSON.stringify(this.saved()));
  /** Colors the server would reject are sent empty so the preview falls back to the defaults. */
  readonly previewBrand = computed<MailingBrandInput>(() => {
    const form = this.form();
    const colors = { ...form.colors };
    for (const field of COLOR_FIELDS) {
      if (!HEX_COLOR.test(colors[field.key])) colors[field.key] = '';
    }
    return { ...form, name: form.name.trim(), colors };
  });

  ngOnInit(): void {
    this.businessesApi
      .list()
      .pipe(takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (page) => {
          const businesses = page.items ?? [];
          this.businesses.set(businesses);
          const businessId = this.current.businessId() ?? businesses[0]?.id;
          if (businessId) this.selectBusiness(businessId);
        },
        error: () => this.error.set('Could not load businesses'),
      });
  }

  selectBusiness(businessId: string): void {
    if (businessId === this.businessId()) return;
    this.businessChanged.next();
    this.businessId.set(businessId);
    if (businessId) this.current.set(businessId);
    this.setup.set(null);
    this.profile.set(null);
    this.brand.set(null);
    this.form.set(emptyForm(''));
    this.saved.set(null);
    this.saving.set(false);
    this.deleting.set(false);
    this.uploadingLogo.set(false);
    this.removingLogo.set(false);
    this.confirmDelete.set(false);
    this.error.set('');
    this.loading.set(false);
    if (!businessId) return;
    this.load(businessId);
  }

  busy(): boolean {
    return this.saving() || this.deleting() || this.uploadingLogo() || this.removingLogo();
  }

  colorValid(key: keyof MailingBrandColors): boolean {
    return HEX_COLOR.test(this.form().colors[key]);
  }

  logoWidthValid(): boolean {
    const width = this.form().logo_width;
    return width === undefined || (Number.isInteger(width) && width >= 40 && width <= 600);
  }

  canSave(): boolean {
    return (
      !!this.businessId() &&
      !this.busy() &&
      !!this.form().name.trim() &&
      COLOR_FIELDS.every((field) => this.colorValid(field.key)) &&
      this.logoWidthValid() &&
      (this.brand() === null || this.dirty())
    );
  }

  /** Empty when a logo may be uploaded; otherwise the reason the uploader is disabled. */
  logoUploadBlocker(): string {
    const check = this.setup()?.checks.find((item) => item.id === 'blob_storage');
    if (check && check.status === 'blocked') {
      return `Logo storage is not configured on this instance. ${check.action || 'Ask an administrator to configure blob storage.'}`;
    }
    if (!this.brand()) return 'Save the brand before uploading a logo.';
    return '';
  }

  patch(changes: Partial<MailingBrandInput>): void {
    this.form.update((form) => ({ ...form, ...changes }));
  }

  pickColor(key: keyof MailingBrandColors, event: Event): void {
    // The native color picker always reports a lowercase #rrggbb value.
    this.patchColor(key, (event.target as HTMLInputElement).value);
  }

  patchColor(key: keyof MailingBrandColors, value: string): void {
    const match = LOOSE_HEX_COLOR.exec(value.trim());
    const color = match ? `#${match[1].toLowerCase()}` : value;
    this.form.update((form) => ({ ...form, colors: { ...form.colors, [key]: color } }));
  }

  patchLogoWidth(value: unknown): void {
    const width = typeof value === 'number' && Number.isFinite(value) ? Math.round(value) : undefined;
    this.patch({ logo_width: width });
  }

  save(): void {
    if (!this.canSave()) return;
    const businessId = this.businessId();
    const form = this.form();
    const body: MailingBrandInput = {
      name: form.name.trim(),
      colors: { ...form.colors },
      font_stack: form.font_stack,
      footer_markdown: form.footer_markdown,
    };
    if (form.logo_width !== undefined) body.logo_width = form.logo_width;
    this.saving.set(true);
    this.mailing
      .putBrand(businessId, body)
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (brand) => {
          this.saving.set(false);
          this.applyBrand(brand);
          this.toast.success('Brand saved');
        },
        error: (error: HttpErrorResponse) => {
          this.saving.set(false);
          this.toast.error(this.describeError(error, 'Could not save the brand'));
        },
      });
  }

  remove(): void {
    if (!this.brand() || this.busy()) return;
    this.deleting.set(true);
    this.mailing
      .deleteBrand(this.businessId())
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: () => {
          this.deleting.set(false);
          this.confirmDelete.set(false);
          this.brand.set(null);
          this.saved.set(null);
          this.form.set(emptyForm(this.profile()?.from_name ?? ''));
          this.toast.success('Brand deleted');
        },
        error: (error: HttpErrorResponse) => {
          this.deleting.set(false);
          this.toast.error(this.describeError(error, 'Could not delete the brand'));
        },
      });
  }

  onLogoSelected(event: Event): void {
    const input = event.target as HTMLInputElement;
    const file = input.files?.[0];
    input.value = '';
    if (file) this.uploadLogo(file);
  }

  uploadLogo(file: File | Blob): void {
    if (this.logoUploadBlocker() || this.busy()) return;
    if (file.size > MAILING_BRAND_LOGO_MAX_BYTES) {
      this.toast.error('Logo must be 512 KB or smaller');
      return;
    }
    this.uploadingLogo.set(true);
    this.mailing
      .uploadBrandLogo(this.businessId(), file)
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (brand) => {
          this.uploadingLogo.set(false);
          this.applyLogo(brand);
          this.toast.success('Logo uploaded');
        },
        error: (error: HttpErrorResponse) => {
          this.uploadingLogo.set(false);
          this.toast.error(this.describeError(error, 'Could not upload the logo'));
        },
      });
  }

  removeLogo(): void {
    if (!this.brand()?.logo || this.busy()) return;
    this.removingLogo.set(true);
    this.mailing
      .deleteBrandLogo(this.businessId())
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: (brand) => {
          this.removingLogo.set(false);
          this.applyLogo(brand);
          this.toast.success('Logo removed');
        },
        error: (error: HttpErrorResponse) => {
          this.removingLogo.set(false);
          this.toast.error(this.describeError(error, 'Could not remove the logo'));
        },
      });
  }

  private load(businessId: string): void {
    this.loading.set(true);
    forkJoin({
      setup: this.mailing.getSetup(businessId).pipe(catchError(() => of(null))),
      brand: this.mailing.getBrand(businessId).pipe(
        catchError((error: HttpErrorResponse) =>
          error.status === 404 ? of(null) : throwError(() => error),
        ),
      ),
      profile: this.mailing.getSendingProfile(businessId).pipe(catchError(() => of(null))),
    })
      .pipe(takeUntil(this.businessChanged), takeUntilDestroyed(this.destroyRef))
      .subscribe({
        next: ({ setup, brand, profile }) => {
          this.setup.set(setup);
          this.profile.set(profile);
          if (brand) this.applyBrand(brand);
          else this.form.set(emptyForm(profile?.from_name ?? ''));
          this.loading.set(false);
        },
        error: (error: HttpErrorResponse) => {
          this.loading.set(false);
          this.error.set(this.describeError(error, 'Could not load the brand'));
        },
      });
  }

  private applyBrand(brand: MailingBrand): void {
    this.brand.set(brand);
    this.confirmDelete.set(false);
    const form: MailingBrandInput = {
      name: brand.name,
      colors: { ...brand.colors },
      font_stack: brand.font_stack,
      footer_markdown: brand.footer_markdown,
      logo_width: brand.logo_width,
    };
    this.form.set(form);
    this.saved.set(form);
  }

  /** Logo changes only touch the stored width; unsaved edits to the other fields are kept. */
  private applyLogo(brand: MailingBrand): void {
    this.brand.set(brand);
    this.form.update((form) => ({ ...form, logo_width: brand.logo_width }));
    this.saved.update((saved) => (saved ? { ...saved, logo_width: brand.logo_width } : saved));
  }

  private describeError(error: HttpErrorResponse, fallback: string): string {
    if (error.status === 400) {
      return (
        (error.error as { message?: string } | null)?.message ||
        'Check the brand fields and try again.'
      );
    }
    if (error.status === 403)
      return 'You do not have permission for this action. Ask a business administrator for mailing permissions.';
    if (error.status === 413) return 'Logo must be 512 KB or smaller';
    return fallback;
  }
}

function emptyForm(name: string): MailingBrandInput {
  return {
    name,
    colors: { ...MAILING_BRAND_DEFAULT_COLORS },
    font_stack: 'system',
    footer_markdown: '',
  };
}
