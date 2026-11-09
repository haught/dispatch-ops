<script lang="ts">
	import type { Job } from '#lib/api/client.js';
	import StatusBadge from '#lib/components/StatusBadge.svelte';
	import { fmtDuration, fmtRelative } from '#lib/format.js';

	/** The latest jobs, newest first. */
	let { jobs, now }: { jobs: Job[]; now: number } = $props();

	const launcher = (j: Job) =>
		j.schedule
			? `schedule ${j.schedule.name}`
			: j.syncInventoryId && !j.launchedBy
				? 'inventory sync'
				: (j.launchedBy?.username ?? j.launchType);
</script>

<section class="card">
	<div class="head">
		<h2>Recent activity</h2>
		<a href="/jobs" class="small">All jobs →</a>
	</div>
	{#if jobs.length === 0}
		<p class="muted">No jobs yet. <a href="/templates">Launch a job template</a> to get started.</p>
	{:else}
		<div class="table-wrap">
			<table class="small">
				<tbody>
					{#each jobs as job (job.id)}
						<tr>
							<td class="name"><a href="/jobs/{job.id}">{job.name}</a></td>
							<td><StatusBadge status={job.status} /></td>
							<td class="muted">{launcher(job)}</td>
							<td class="num">{job.startedAt ? fmtDuration(job.startedAt, job.finishedAt) : '—'}</td
							>
							<td class="muted num" title={new Date(job.createdAt).toLocaleString()}>
								{fmtRelative(job.createdAt, now)}
							</td>
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
	}

	td {
		padding: 0.35rem 0.4rem;
	}

	tr:last-child td {
		border-bottom: none;
	}

	.name {
		max-width: 16rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.num {
		white-space: nowrap;
		text-align: right;
		font-variant-numeric: tabular-nums;
	}
</style>
