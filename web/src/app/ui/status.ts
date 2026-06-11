export type Tone = 'neutral' | 'accent' | 'warn' | 'success' | 'danger';

export function ticketStatusTone(s: string): Tone {
  switch (s) {
    case 'new': case 'open': return 'accent';
    case 'pending': return 'warn';
    case 'solved': return 'success';
    case 'closed': default: return 'neutral';
  }
}

export function ticketPriorityTone(p: string): Tone {
  switch (p) {
    case 'urgent': return 'danger';
    case 'high': return 'warn';
    default: return 'neutral';
  }
}

export function runStatusTone(s: string): Tone {
  switch (s) {
    case 'succeeded': case 'completed': return 'success';
    case 'failed': case 'error': return 'danger';
    case 'running': case 'pending': return 'accent';
    default: return 'neutral';
  }
}
