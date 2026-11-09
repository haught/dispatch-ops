<script lang="ts">
	/**
	 * A headline number with its change since the previous window. The change
	 * is shown with an arrow and words, so it never relies on color alone.
	 */
	let {
		label,
		value,
		current,
		previous,
		better = 'up',
		points = false,
		period
	}: {
		label: string;
		/** The formatted value. */
		value: string;
		/** The raw values compared for the change (null if unknown). */
		current: number | null | undefined;
		previous: number | null | undefined;
		/** Which way is good; 'neutral' shows the change without judging it. */
		better?: 'up' | 'down' | 'neutral';
		/** Compare in percentage points (for rates) rather than relative change. */
		points?: boolean;
		/** The comparison period, e.g. "prev 7d". */
		period: string;
	} = $props();

	const change = $derived.by(() => {
		if (current == null || previous == null) return null;
		if (points) {
			const d = (current - previous) * 100;
			return Math.abs(d) < 0.05
				? { dir: 0, text: '0 pts' }
				: { dir: Math.sign(d), text: `${Math.abs(d).toFixed(1)} pts` };
		}
		if (previous === 0) return current === 0 ? { dir: 0, text: '0%' } : null;
		const d = ((current - previous) / previous) * 100;
		return Math.abs(d) < 0.5
			? { dir: 0, text: '0%' }
			: {
					dir: Math.sign(d),
					text: `${Math.abs(d) >= 10 ? Math.round(Math.abs(d)) : Math.abs(d).toFixed(1)}%`
				};
	});

	const tone = $derived(
		!change || change.dir === 0 || better === 'neutral'
			? 'neutral'
			: change.dir > 0 === (better === 'up')
				? 'good'
				: 'bad'
	);
</script>

<div class="tile">
	<div class="label">{label}</div>
	<div class="value">{value}</div>
	<div class="change {tone}">
		{#if change}
			<span aria-hidden="true">{change.dir > 0 ? '▲' : change.dir < 0 ? '▼' : '■'}</span>
			<span class="sr-only">{change.dir > 0 ? 'up' : change.dir < 0 ? 'down' : 'unchanged'}</span>
			{change.text} <span class="muted">vs {period}</span>
		{:else}
			<span class="muted">no data for {period}</span>
		{/if}
	</div>
</div>

<style>
	.tile {
		background: var(--surface);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: 0.85rem 1rem;
		min-width: 0;
	}

	.label {
		color: var(--muted);
		font-size: 0.85rem;
	}

	.value {
		font-size: 1.6rem;
		font-weight: 600;
		line-height: 1.3;
		font-variant-numeric: tabular-nums;
	}

	.change {
		font-size: 0.8rem;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.good {
		color: var(--ok);
	}

	.bad {
		color: var(--danger);
	}

	.neutral {
		color: var(--muted);
	}

	.change .muted {
		color: var(--muted);
	}

	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		overflow: hidden;
		clip: rect(0 0 0 0);
	}
</style>
