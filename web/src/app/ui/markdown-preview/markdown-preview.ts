import { AfterViewInit, Component, ElementRef, Input, OnChanges, ViewChild } from '@angular/core';

@Component({
  selector: 'mf-markdown-preview',
  standalone: true,
  template: `
    <iframe
      #frame
      sandbox=""
      title="Email preview"
      referrerpolicy="no-referrer"
      data-testid="mailing-preview-frame"
    ></iframe>
  `,
  styles: [
    `
      :host {
        display: block;
        color-scheme: light;
      }
      iframe {
        display: block;
        width: 100%;
        height: 70vh;
        border: 1px solid var(--mf-border);
        border-radius: var(--mf-radius-sm);
        background: white;
      }
    `,
  ],
})
export class MarkdownPreview implements AfterViewInit, OnChanges {
  @Input() html = '';
  @ViewChild('frame') private frame?: ElementRef<HTMLIFrameElement>;

  ngAfterViewInit(): void {
    this.writeSrcdoc();
  }

  ngOnChanges(): void {
    this.writeSrcdoc();
  }

  private writeSrcdoc(): void {
    if (this.frame) this.frame.nativeElement.srcdoc = buildPreviewDocument(this.html);
  }
}

export function buildPreviewDocument(html: string): string {
  const document = new DOMParser().parseFromString(html, 'text/html');
  const csp = document.createElement('meta');
  csp.httpEquiv = 'Content-Security-Policy';
  csp.content = "default-src 'none'; style-src 'unsafe-inline'; img-src data:";
  const colorScheme = document.createElement('meta');
  colorScheme.name = 'color-scheme';
  colorScheme.content = 'light';
  document.head.prepend(csp, colorScheme);
  return `<!doctype html>${document.documentElement.outerHTML}`;
}
