<script lang="ts">
	import type { JobStatus } from '#lib/api/client.js';

	/**
	 * A job template's latest results as a row of marks, oldest on the left.
	 * Each outcome has its own shape as well as color: a dot for success, a
	 * cross for failure, a ring while active, a dash when canceled.
	 */
	let { statuses }: { statuses: JobStatus[] } = $props();

	const marks = $derived([...statuses].reverse());

	const kind = (s: JobStatus) =>
		s === 'successful'
			? 'ok'
			: s === 'failed' || s === 'error'
				? 'fail'
				: s === 'canceled'
					? 'canceled'
					: 'active';

	const summary = $derived.by(() => {
		const n = { ok: 0, fail: 0, canceled: 0, active: 0 };
		for (const s of statuses) n[kind(s)]++;
		const parts = [
			n.ok && `${n.ok} successful`,
			n.fail && `${n.fail} failed`,
			n.active && `${n.active} active`,
			n.canceled && `${n.canceled} canceled`
		].filter(Boolean);
		return `Last ${statuses.length} runs: ${parts.join(', ')}`;
	});
</script>

<span class="strip" role="img" aria-label={summary}>
	{#each marks as s, i (i)}
		<svg viewBox="0 0 12 12" width="12" height="12" class={kind(s)} aria-hidden="true">
			<title>{s}</title>
			{#if kind(s) === 'ok'}
				<circle cx="6" cy="6" r="4.5" />
			{:else if kind(s) === 'fail'}
				<path d="M2.5 2.5 L9.5 9.5 M9.5 2.5 L2.5 9.5" />
			{:else if kind(s) === 'canceled'}
				<path d="M2.5 6 H9.5" />
			{:else}
				<circle cx="6" cy="6" r="3.75" class="ring" />
			{/if}
		</svg>
	{/each}
</span>

<style>
	.strip {
		display: inline-flex;
		gap: 3px;
		align-items: center;
		vertical-align: middle;
	}

	.ok circle {
		fill: var(--ok);
	}

	.fail path {
		stroke: var(--danger);
		stroke-width: 2.5;
		stroke-linecap: round;
	}

	.canceled path {
		stroke: var(--muted);
		stroke-width: 2.5;
		stroke-linecap: round;
	}

	.ring {
		fill: none;
		stroke: var(--accent);
		stroke-width: 2;
	}
</style>
