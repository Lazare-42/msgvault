import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { OperationsController } from '../../operations/controller.svelte';
import type { OperationRunSummary, OperationsURLState } from '../../operations/models';
import OperationsWorkspace from './OperationsWorkspace.svelte';

// The daemon encrypts run IDs with a fresh nonce per response
// (internal/api/operation_tokens.go), so the same run never carries the same
// ID twice. These fixtures do the same; a test cannot pass by matching IDs.
let encodings = 0;
const encode = (run: number) => `op2.${String(++encodings).padStart(32, '0')}.run${run}`;

function summary(run: number): OperationRunSummary {
  return {
    id: encode(run), kind: 'source_sync', lane: 'messages', trigger: 'manual', state: 'succeeded',
    started_at: `2026-08-30T1${run}:00:00Z`, finished_at: `2026-08-30T1${run}:01:00Z`,
    counters: [{ name: 'processed', unit: 'messages', value: run }]
  };
}

const state = (overrides: Partial<OperationsURLState> = {}): OperationsURLState => ({
  operationLane: '', operationKind: '', operationState: '', operationStartedFrom: '',
  operationStartedBefore: '', operationRunID: null, operationStatus: '', ...overrides
});

function daemon(pageTwo: () => Response = () => Response.json({
  runs: [summary(3), summary(4)], membership_revision: 7, unavailable_kinds: []
})) {
  const requests: string[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    requests.push(`${url.pathname}${url.search}`);
    if (url.pathname === '/api/v1/operations/status') {
      return Response.json({ lanes: [{ lane: 'messages', kind: 'source_sync', configured: true,
        history_availability: 'available', supported_actions: [], latest: summary(1) }] });
    }
    if (url.pathname === '/api/v1/operations/runs') {
      if (url.searchParams.get('cursor') === 'page-two') return pageTwo();
      return Response.json({
        runs: [summary(1), summary(2)], membership_revision: 7, unavailable_kinds: [], next_cursor: 'page-two'
      });
    }
    return Response.json({ ...summary(3), related_status: 'listSourceStatus', supported_actions: [] });
  });
  return { requests, client: createAPIClient(fetchFn) };
}

const runButtons = () => screen.getAllByRole('button', { name: 'Open Source sync run' });

afterEach(() => vi.useRealTimers());

describe('Operations refresh', () => {
  it('refreshes only status on its timer and on click, keeping paged rows, detail, and focus', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { requests, client } = daemon();
    const controller = new OperationsController(client);
    try {
      await controller.applyURLState(state());
      await controller.loadMore();
      const selected = controller.snapshot.rows[2]!.id;
      await controller.applyURLState(state({ operationRunID: selected }));
      render(OperationsWorkspace, { controller, state: state({ operationRunID: selected }) });
      expect(runButtons()).toHaveLength(4);
      expect(screen.getByRole('region', { name: 'Operation run detail' })).toBeDefined();
      runButtons()[2]!.focus();
      const focused = document.activeElement;

      requests.length = 0;
      await vi.advanceTimersByTimeAsync(5 * 60 * 1000);
      await waitFor(() => expect(requests).toEqual(['/api/v1/operations/status']));
      expect(runButtons()).toHaveLength(4);
      expect(screen.getByRole('region', { name: 'Operation run detail' })).toBeDefined();
      expect(document.activeElement).toBe(focused);

      requests.length = 0;
      await fireEvent.click(screen.getByRole('button', { name: 'Refresh operation status' }));
      await waitFor(() => expect(requests).toEqual(['/api/v1/operations/status']));
      expect(runButtons()).toHaveLength(4);
      expect(screen.getByRole('region', { name: 'Operation run detail' })).toBeDefined();
    } finally {
      controller.destroy();
    }
  });

  it('reloads status and page one of runs from Reload run history', async () => {
    const { requests, client } = daemon();
    const controller = new OperationsController(client);
    try {
      await controller.applyURLState(state());
      await controller.loadMore();
      const before = controller.snapshot.rows.map((row) => row.id);
      render(OperationsWorkspace, { controller, state: state() });
      requests.length = 0;
      await fireEvent.click(screen.getByRole('button', { name: 'Reload run history' }));
      await waitFor(() => expect(runButtons()).toHaveLength(2));
      expect(requests.sort()).toEqual(['/api/v1/operations/runs?limit=25', '/api/v1/operations/status']);
      expect(controller.snapshot.rows.some((row) => before.includes(row.id))).toBe(false);
    } finally {
      controller.destroy();
    }
  });

  it('keeps the conflict notice when Load more meets a changed history', async () => {
    const { client } = daemon(() => Response.json(
      { error: 'operation_history_conflict', message: 'Operation history changed.' }, { status: 409 }));
    const controller = new OperationsController(client);
    try {
      await controller.applyURLState(state());
      render(OperationsWorkspace, { controller, state: state() });
      await fireEvent.click(screen.getByRole('button', { name: 'Load more operation history' }));
      const conflict = await screen.findByRole('alert', { name: 'Operation history conflict' });
      expect(conflict.textContent).toContain('Operation history changed. Restart from the first page.');
      expect(screen.getByRole('button', { name: 'Restart operation history' })).toBeDefined();
    } finally {
      controller.destroy();
    }
  });
});
