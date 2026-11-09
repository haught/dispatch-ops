<script lang="ts">
	import type { TemplateStats } from '#lib/api/client.js';
	import { fmtPercent, fmtSeconds } from '#lib/format.js';
	import StatusStrip from './StatusStrip.svelte';

	/** The busiest job templates in the window, sortable by runs, failures or run time. */
	let { templates, period }: { templates: TemplateStats[]; period: string } = $props();

	type SortKey = 'runs' | 'failures' | 'slowest';
	let sort = $state<SortKey>('runs');

	const sorted = $derived(
		[...templates]
			.sort((a, b) =>
				sort === 'runs'
					? b.runs - a.runs
					: sort === 'failures'
						? b.failures - a.failures || b.failures / b.runs - a.failures / a.runs
						: (b.p50DurationSeconds ?? -1) - (a.p50DurationSeconds ?? -1)
			)
			.filter((t) => sort !== 'failures' || t.failures > 0)
			.slice(0, 8)
	);
</script>

<section class="card">
	<div class="head">
		<h2>Job templates</h2>
		<div class="seg" role="group" aria-label="Sort templates by">
			{#each [['runs', 'Most run'], ['failures', 'Most failing'], ['slowest', 'Slowest']] as [key, label] (key)}
				<button
					class="secondary"
					class:active={sort === key}
					aria-pressed={sort === key}
					onclick={() => (sort = key as SortKey)}>{label}</button
				>
			{/each}
		</div>
	</div>
	{#if templates.length === 0}
		<p class="muted">No template runs in the {period}.</p>
	{:else if sorted.length === 0}
		<p class="muted">No template failed in the {period}.</p>
	{:else}
		<div class="table-wrap">
			<table class="small">
				<thead>
					<tr>
						<th>Template</th>
						<th class="num">Runs</th>
						<th class="num">Failed</th>
						<th class="num">Median</th>
						<th>Last 10 runs</th>
					</tr>
				</thead>
				<tbody>
					{#each sorted as t (t.id)}
						<tr>
							<td class="name"><a href="/templates/{t.id}">{t.name}</a></td>
							<td class="num">{t.runs}</td>
							<td class="num" class:bad={t.failures > 0}>
								{t.failures}
								{#if t.failures > 0}<span class="muted">({fmtPercent(t.failures / t.runs)})</span
									>{/if}
							</td>
							<td class="num"
								>{t.p50DurationSeconds == null ? '—' : fmtSeconds(t.p50DurationSeconds)}</td
							>
							<td><StatusStrip statuses={t.recent} /></td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<style>
	.head {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
		flex-wrap: wrap;
		gap: 0.5rem;
		margin-bottom: 0.5rem;
	}

	.head h2 {
		margin: 0;
	}

	.seg {
		display: inline-flex;
	}

	.seg button {
		padding: 0.2rem 0.6rem;
		font-size: 0.8rem;
		border-radius: 0;
	}

	.seg button:first-child {
		border-radius: 6px 0 0 6px;
	}

	.seg button:last-child {
		border-radius: 0 6px 6px 0;
	}

	.seg button + button {
		border-left: none;
	}

	.seg button.active {
		background: var(--accent);
		border-color: var(--accent);
		color: var(--accent-text);
	}

	td,
	th {
		padding: 0.35rem 0.4rem;
	}

	tr:last-child td {
		border-bottom: none;
	}

	.name {
		max-width: 14rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.num {
		text-align: right;
		white-space: nowrap;
		font-variant-numeric: tabular-nums;
	}

	.bad {
		color: var(--danger);
	}
</style>
