import { describe, expect, it } from 'vitest';
import { ticketPriorityTone, ticketStatusTone, runStatusTone, Tone, effectClassTone, effectClassLabel } from './status';

describe('status tone helpers', () => {
  it('maps ticket status to a tone', () => {
    expect(ticketStatusTone('new')).toBe<Tone>('accent');
    expect(ticketStatusTone('open')).toBe<Tone>('accent');
    expect(ticketStatusTone('pending')).toBe<Tone>('warn');
    expect(ticketStatusTone('solved')).toBe<Tone>('success');
    expect(ticketStatusTone('closed')).toBe<Tone>('neutral');
  });
  it('maps priority to a tone', () => {
    expect(ticketPriorityTone('urgent')).toBe<Tone>('danger');
    expect(ticketPriorityTone('high')).toBe<Tone>('warn');
    expect(ticketPriorityTone('normal')).toBe<Tone>('neutral');
    expect(ticketPriorityTone('low')).toBe<Tone>('neutral');
  });
  it('maps run status to a tone', () => {
    expect(runStatusTone('succeeded')).toBe<Tone>('success');
    expect(runStatusTone('failed')).toBe<Tone>('danger');
    expect(runStatusTone('running')).toBe<Tone>('accent');
  });
});

describe('effect class mapping', () => {
  it('maps effect class int to tone', () => {
    expect(effectClassTone(0)).toBe('neutral');
    expect(effectClassTone(1)).toBe('accent');
    expect(effectClassTone(2)).toBe('warn');
    expect(effectClassTone(3)).toBe('danger');
  });
  it('labels effect classes', () => {
    expect(effectClassLabel(0)).toBe('Read');
    expect(effectClassLabel(1)).toBe('Reversible');
    expect(effectClassLabel(2)).toBe('External');
    expect(effectClassLabel(3)).toBe('Irreversible');
  });
});
