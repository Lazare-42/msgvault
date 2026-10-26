<script lang="ts">
  import { Button, Chip, type ChipTone } from '@kenn-io/kit-ui';

  import {
    OPERATION_ACTION_LABELS,
    OPERATION_KIND_LABELS,
    OPERATION_LANE_LABELS,
    OPERATION_SETUP,
    RELATED_STATUS_LABELS,
    operationStateChip,
    type OperationSettingsTarget,
    type RelatedStatus
  } from '../../operations/labels';
  import type {
    OperationAction,
    OperationLaneStatus,
    OperationStatusLane
  } from '../../operations/models';
  import { formatDateTime } from '../../util/format';
  import OperationHostSetup from './OperationHostSetup.svelte';

  let {
    lanes,
    actionPending = null,
    onNavigate = () => undefined,
    onAction = () => undefined,
    onSetUp = () => undefined
  }: {
    lanes: readonly OperationStatusLane[];
    actionPending?: OperationAction | null;
    onNavigate?: (target: RelatedStatus, button: HTMLButtonElement) => void;
    onAction?: (action: OperationAction) => void;
    onSetUp?: (target: OperationSettingsTarget) => void;
  } = $props();

  function statusChip(kind: OperationLaneStatus): { label: string; tone: ChipTone } | undefined {
    if (kind.active) return operationStateChip(kind.active.state);
    if (!kind.configured) return { label: 'Off', tone: 'muted' };
    if (kind.latest) return operationStateChip(kind.latest.state);
    return kind.history_availability === 'available' ? { label: 'No runs yet', tone: 'muted' } : undefined;
  }
</script>

<section class="status-list" aria-label="Operation lanes">
  {#each lanes as lane (lane.lane)}
    {@const headingID = `operation-lane-${lane.lane}`}
    <div class="lane">
      <h2 id={headingID}>{OPERATION_LANE_LABELS[lane.lane]}</h2>
      {#if lane.kinds.length === 0}
        <Chip size="sm" tone="muted" uppercase={false}>Status unavailable</Chip>
      {:else}
        <ul aria-labelledby={headingID}>
          {#each lane.kinds as kind (kind.kind)}
            {@const nameID = `operation-kind-${kind.kind}`}
            {@const chip = statusChip(kind)}
            {@const run = kind.active ?? kind.latest}
            {@const setup = kind.configured ? undefined : OPERATION_SETUP[kind.kind]}
            <li aria-labelledby={nameID}>
              <span class="name" id={nameID}>{OPERATION_KIND_LABELS[kind.kind]}</span>
              <span class="status">
                {#if chip}<Chip size="sm" tone={chip.tone} uppercase={false}>{chip.label}</Chip>{/if}
                {#if kind.history_availability !== 'available'}
                  <span class="history-note">History unavailable</span>
                {/if}
              </span>
              <span class="time">
                {#if run}<time datetime={run.started_at}>{formatDateTime(run.started_at)}</time>{/if}
                {#if kind.latest_successful && run?.state !== 'succeeded'}
                  <span>
                    Last succeeded
                    <time datetime={kind.latest_successful.started_at}>
                      {formatDateTime(kind.latest_successful.started_at)}
                    </time>
                  </span>
                {/if}
              </span>
              <span class="actions">
                {#if kind.related_status}
                  <Button
                    size="sm"
                    surface="soft"
                    label={`Open ${RELATED_STATUS_LABELS[kind.related_status]}`}
                    onclick={(event) => onNavigate(kind.related_status!, event.currentTarget as HTMLButtonElement)}
                  />
                {/if}
                {#each kind.supported_actions as action (action)}
                  <Button
                    size="sm"
                    tone="info"
                    label={actionPending === action ? `${OPERATION_ACTION_LABELS[action]}…` : OPERATION_ACTION_LABELS[action]}
                    disabled={actionPending !== null}
                    onclick={() => onAction(action)}
                  />
                {/each}
                {#if setup?.kind === 'settings'}
                  <Button
                    size="sm"
                    tone="info"
                    surface="soft"
                    label="Set up"
                    ariaLabel={`Set up ${OPERATION_KIND_LABELS[kind.kind]}`}
                    onclick={() => onSetUp(setup.target)}
                  />
                {:else if setup?.kind === 'host'}
                  <OperationHostSetup {setup} />
                {/if}
              </span>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/each}
</section>

<style>
  .status-list { display: grid; gap: var(--space-4); min-width: 0; }
  .lane { display: grid; gap: var(--space-2); min-width: 0; }
  h2 { margin: 0; font-size: var(--font-size-sm); }
  ul { display: grid; margin: 0; padding: 0; list-style: none; }
  li {
    display: grid;
    grid-template-columns: minmax(10rem, 14rem) minmax(7rem, 10rem) minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-2) var(--space-3);
    padding-block: var(--space-2);
    border-top: 1px solid var(--border-muted);
  }
  .name { min-width: 0; font-size: var(--font-size-sm); font-weight: 600; }
  .status, .actions { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-2); }
  .actions { justify-content: flex-end; min-width: 0; max-width: 28rem; }
  .history-note { color: var(--status-warning-ink); font-size: var(--font-size-xs); }
  .time { display: grid; gap: var(--space-1); color: var(--text-muted); font-size: var(--font-size-xs); }

  @media (max-width: 760px) {
    li { grid-template-columns: 1fr; }
    .actions { justify-content: flex-start; }
  }
</style>
