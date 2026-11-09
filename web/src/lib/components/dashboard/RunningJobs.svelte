<script lang="ts">
	import type { Job } from '#lib/api/client.js';
	import StatusBadge from '#lib/components/StatusBadge.svelte';
	import { fmtDuration } from '#lib/format.js';

	/** Jobs that are running, starting or queued, with how long they have taken so far. */
	let { jobs, now }: { jobs: Job[]; now: number } = $props();
</script>

<section class="card">
	<h2>Running &amp; queued <span class="count">{jobs.length}</span></h2>
	{#if jobs.length === 0}
		<p class="muted">Nothing is running right now.</p>
	{:else}
		<ul>
			{#each jobs as job (job.id)}
				{@const since = job.startedAt ?? job.createdAt}
				<li>
					<a href="/jobs/{job.id}" class="name">{job.name}</a>
					<StatusBadge status={job.status} />
					<span class="muted small time" title={job.startedAt ? 'Running for' : 'Queued for'}>
						{job.startedAt ? '' : 'queued '}{fmtDuration(since, new Date(now).toISOString())}
					</span>
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	li {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		padding: 0.35rem 0;
		border-bottom: 1px solid var(--border);
	}

	li:last-child {
		border-bottom: none;
	}

	.name {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.time {
		font-variant-numeric: tabular-nums;
		white-space: nowrap;
	}

	.count {
		color: var(--muted);
		font-weight: normal;
		font-size: 0.9rem;
	}
</style>
