<script lang="ts">
  import type { OperationHostSetup } from '../../operations/labels';

  let { setup }: { setup: OperationHostSetup } = $props();
</script>

<p class="host-setup">
  {setup.text}
  <a href={setup.guideHref} target="_blank" rel="noreferrer">{setup.guideLabel}</a>
</p>

<style>
  .host-setup { margin: 0; color: var(--text-muted); font-size: var(--font-size-xs); }
</style>
