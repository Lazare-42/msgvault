import { describe, expect, it } from 'vitest';
import { counterLabel, counterSummary, counterValue, operationDuration, operationStateChip, triggerLabel } from './labels';

describe('operation labels', () => {
  it.each([
    ['queued', 'Queued', 'info'], ['running', 'Running', 'info'], ['succeeded', 'Succeeded', 'success'],
    ['partial', 'Partial', 'warning'], ['failed', 'Failed', 'danger'], ['cancelled', 'Cancelled', 'muted'],
    ['future_state', 'Future state', 'neutral']
  ])('chips state %s', (state, label, tone) => expect(operationStateChip(state)).toEqual({ label, tone }));

  it('names triggers and shows a dash when none was recorded', () => {
    expect(triggerLabel('manual')).toBe('Manual');
    expect(triggerLabel('scheduled')).toBe('Scheduled');
    expect(triggerLabel(undefined)).toBe('—');
  });

  it('names each unit once, on its first counter, and leaves out zeros', () => {
    expect(counterSummary([
      { name: 'processed', unit: 'messages', value: 20 },
      { name: 'added', unit: 'messages', value: 20 },
      { name: 'item_errors', unit: 'messages', value: 0 },
      { name: 'updated', unit: 'people', value: 3 }
    ])).toBe('20 messages processed · 20 added · 3 people updated');
  });

  it('reads counters named for their unit and single values naturally', () => {
    expect(counterSummary([{ name: 'projected_writes', unit: 'writes', value: 3 }])).toBe('3 projected writes');
    expect(counterSummary([{ name: 'books', unit: 'books', value: 1 }])).toBe('1 book');
    expect(counterSummary([{ name: 'processed', unit: 'messages', value: 1 }])).toBe('1 message processed');
  });

  it('says No counters when nothing is left to show', () => {
    expect(counterSummary([])).toBe('No counters');
    expect(counterSummary([{ name: 'failed', unit: 'messages', value: 0 }])).toBe('No counters');
  });

  it('labels detail counters in sentence case with every value', () => {
    expect(counterLabel('item_errors')).toBe('Item errors');
    expect(counterValue({ name: 'item_errors', unit: 'messages', value: 0 })).toBe('0 messages');
  });

  it('measures durations', () => {
    expect(operationDuration({ state: 'succeeded', started_at: '2026-08-30T10:00:00Z', finished_at: '2026-08-30T10:01:05Z' }))
      .toBe('1 minute 5 seconds');
    expect(operationDuration({ state: 'running', started_at: '2026-08-30T10:00:00Z' })).toBe('In progress');
    expect(operationDuration({ state: 'failed', started_at: '2026-08-30T10:00:00Z' })).toBe('Not available');
  });
});
