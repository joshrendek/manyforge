import { TestBed } from '@angular/core/testing';
import { describe, expect, it } from 'vitest';
import { MarkdownPreview } from './markdown-preview';

describe('MarkdownPreview', () => {
  it('writes style-bearing HTML to a strictly sandboxed iframe imperatively', () => {
    const fixture = TestBed.createComponent(MarkdownPreview);
    const html = '<!doctype html><style>body{color:tomato}</style><p>Hello</p>';
    fixture.componentRef.setInput('html', html);
    fixture.detectChanges();

    const frame = fixture.nativeElement.querySelector('iframe') as HTMLIFrameElement;
    expect(frame.srcdoc).toContain('<style>body{color:tomato}</style>');
    expect(frame.srcdoc).toContain('Content-Security-Policy');
    expect(frame.srcdoc).toContain("default-src 'none'");
    expect(frame.getAttribute('sandbox')).toBe('');
    expect(frame.getAttribute('referrerpolicy')).toBe('no-referrer');
    expect(frame.getAttribute('title')).toBe('Email preview');

    fixture.componentRef.setInput('html', '<style>p{font-weight:bold}</style><p>Updated</p>');
    fixture.detectChanges();
    expect(frame.srcdoc).toContain('font-weight:bold');
  });

  it('injects a policy that blocks remote preview subresources', () => {
    const fixture = TestBed.createComponent(MarkdownPreview);
    fixture.componentRef.setInput(
      'html',
      '<img src="http://127.0.0.1/private"><style>@import "https://example.test/x.css";</style>',
    );
    fixture.detectChanges();
    const frame = fixture.nativeElement.querySelector('iframe') as HTMLIFrameElement;
    expect(frame.srcdoc).toContain("default-src 'none'; style-src 'unsafe-inline'; img-src data:");
    expect(frame.srcdoc).not.toContain('img-src http:');
  });
});
