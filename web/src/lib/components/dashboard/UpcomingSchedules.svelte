<script lang="ts">
	import type { Schedule } from '#lib/api/client.js';
	import { fmtRelative } from '#lib/format.js';

	/** The next scheduled runs, plus schedules whose last run went wrong. */
	let { schedules, now }: { schedules: Schedule[]; now: number } = $props();

	const upcoming = $derived(
		schedules
			.filter((s) => s.enabled && s.nextRunAt)
			.sort((a, b) => new Date(a.nextRunAt!).getTime() - new Date(b.nextRunAt!).getTime())
			.slice(0, 5)
	);
	const broken = $derived(schedules.filter((s) => s.enabled && s.lastError));
	const target = (s: Schedule) =>
		s.jobTemplateName ?? (s.inventoryName ? `sync ${s.inventoryName}` : '');
</script>

<section class="card">
	<div class="head">
		<h2>Upcoming schedules</h2>
		<a href="/schedules" class="small">All schedules →</a>
	</div>
	{#if broken.length}
		<ul class="broken">
			{#each broken as s (s.id)}
				<li>
					<span class="badge warn">⚠ last run failed</span>
					<strong>{s.name}</strong>
					<div class="muted small reason" title={s.lastError}>{s.lastError}</div>
				</li>
			{/each}
		</ul>
	{/if}
	{#if upcoming.length === 0}
		<p class="muted">
			No enabled schedules. <a href="/schedules">Create one</a> to run templates on a cron.
		</p>
	{:else}
		<ul>
			{#each upcoming as s (s.id)}
				<li class="row">
					<span class="name">
						{s.name}
						{#if target(s)}<span class="muted small">· {target(s)}</span>{/if}
					</span>
					<span class="when small" title={new Date(s.nextRunAt!).toLocaleString()}>
						{fmtRelative(s.nextRunAt, now)}
					</span>
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	.head {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
	}

	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	li {
		padding: 0.35rem 0;
		border-bottom: 1px solid var(--border);
	}

	li:last-child {
		border-bottom: none;
	}

	.broken {
		margin-bottom: 0.5rem;
	}

	.broken li {
		border-bottom: 1px solid var(--border);
	}

	.row {
		display: flex;
		justify-content: space-between;
		gap: 0.5rem;
	}

	.name {
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.when {
		white-space: nowrap;
		font-variant-numeric: tabular-nums;
	}

	.reason {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
</style>
